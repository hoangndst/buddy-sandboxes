package main

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

type copyEndpoint struct {
	isRemote bool
	sandbox  string
	path     string
}

func parseCopyEndpoint(arg string) copyEndpoint {
	if isWindowsDrivePath(arg) {
		return copyEndpoint{path: arg}
	}
	parts := strings.SplitN(arg, ":", 2)
	if len(parts) == 2 && parts[0] != "" {
		p := parts[1]
		if p == "" {
			p = "/workspace"
		} else if !strings.HasPrefix(p, "/") {
			p = "/workspace/" + p
		}
		return copyEndpoint{
			isRemote: true,
			sandbox:  parts[0],
			path:     p,
		}
	}
	return copyEndpoint{
		isRemote: false,
		path:     arg,
	}
}

func isWindowsDrivePath(arg string) bool {
	if len(arg) < 3 || arg[1] != ':' || (arg[2] != '\\' && arg[2] != '/') {
		return false
	}
	return (arg[0] >= 'a' && arg[0] <= 'z') || (arg[0] >= 'A' && arg[0] <= 'Z')
}

func newCpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cp <source> <destination>",
		Short: "Copy files and directories between a local filesystem and a sandbox",
		Long: `cp copies files or directories bidirectionally between the local filesystem and a remote sandbox.
Remote paths must be specified using the format <sandbox-id>:<container-path>.

Examples:
  # Copy local directory into sandbox workspace
  buddy-sbx cp ./my-project sbx-123:/workspace/project

  # Download build artifact from sandbox to local filesystem
  buddy-sbx cp sbx-123:/workspace/dist.tar.gz ./dist.tar.gz`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			src := parseCopyEndpoint(args[0])
			dst := parseCopyEndpoint(args[1])

			if src.isRemote && dst.isRemote {
				return fmt.Errorf("copying directly between two remote sandboxes is not supported")
			}
			if !src.isRemote && !dst.isRemote {
				return fmt.Errorf("at least one argument must be a remote sandbox path (<sandbox-id>:<path>)")
			}

			c, err := getClient()
			if err != nil {
				return err
			}

			ctx := cmd.Context()

			if !src.isRemote && dst.isRemote {
				// Upload: local src -> remote dst
				absSrc, err := filepath.Abs(src.path)
				if err != nil {
					return fmt.Errorf("invalid source path %s: %w", src.path, err)
				}
				sourceInfo, err := os.Stat(absSrc)
				if err != nil {
					return fmt.Errorf("source path %s does not exist: %w", absSrc, err)
				}
				fileDestination := !sourceInfo.IsDir() && !strings.HasSuffix(dst.path, "/")
				directoryDestination := sourceInfo.IsDir() && !strings.HasSuffix(dst.path, "/")
				archiveName := filepath.Base(absSrc)

				pr, pw := io.Pipe()
				go func() {
					var err error
					if fileDestination {
						err = tarPackAs(absSrc, archiveName, pw)
					} else {
						err = tarPack(absSrc, pw)
					}
					_ = pw.CloseWithError(err)
				}()

				if fileDestination {
					err = c.UploadFile(ctx, dst.sandbox, dst.path, archiveName, pr)
				} else if directoryDestination {
					err = c.UploadDirectory(ctx, dst.sandbox, dst.path, archiveName, pr)
				} else {
					err = c.Upload(ctx, dst.sandbox, dst.path, pr)
				}
				if err != nil {
					return fmt.Errorf("upload failed: %w", err)
				}

				fmt.Printf("Successfully copied %s to %s:%s\n", args[0], dst.sandbox, dst.path)
				return nil
			}

			// Download: remote src -> local dst
			absDst, err := filepath.Abs(dst.path)
			if err != nil {
				return fmt.Errorf("invalid destination path %s: %w", dst.path, err)
			}

			rc, err := c.Download(ctx, src.sandbox, src.path)
			if err != nil {
				return fmt.Errorf("download failed: %w", err)
			}
			defer func() { _ = rc.Close() }()

			stageParent, destinationIsDirectory, err := prepareDownloadDestination(absDst, args[1])
			if err != nil {
				return err
			}
			stage, err := os.MkdirTemp(stageParent, ".buddy-sbx-cp-*")
			if err != nil {
				return fmt.Errorf("creating temporary destination failed: %w", err)
			}
			defer func() { _ = os.RemoveAll(stage) }()

			first, dirModes, err := tarUnpackWithInfo(rc, stage)
			if err != nil {
				return fmt.Errorf("extracting archive failed: %w", err)
			}
			if first == nil {
				return fmt.Errorf("download archive is empty")
			}
			if first.Typeflag == tar.TypeDir && filepath.Clean(filepath.FromSlash(first.Name)) == "." {
				destinationIsDirectory = true
			}
			if err := installDownloadedItem(stage, absDst, first.Name, destinationIsDirectory, dirModes); err != nil {
				return err
			}

			fmt.Printf("Successfully copied %s:%s to %s\n", src.sandbox, src.path, args[1])
			return nil
		},
	}

	return cmd
}

func prepareDownloadDestination(dst, userPath string) (string, bool, error) {
	destinationIsDirectory := strings.HasSuffix(userPath, string(os.PathSeparator))
	if info, err := os.Stat(dst); err == nil {
		destinationIsDirectory = info.IsDir()
	} else if !os.IsNotExist(err) {
		return "", false, fmt.Errorf("checking destination failed: %w", err)
	}
	if destinationIsDirectory {
		if err := os.MkdirAll(dst, 0755); err != nil {
			return "", false, fmt.Errorf("creating destination directory failed: %w", err)
		}
		return dst, true, nil
	}
	parent := filepath.Dir(dst)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return "", false, fmt.Errorf("creating destination parent failed: %w", err)
	}
	return parent, false, nil
}

func installDownloadedItem(
	stage, dst, archiveName string,
	destinationIsDirectory bool,
	dirModes map[string]os.FileMode,
) error {
	if destinationIsDirectory {
		if err := os.MkdirAll(dst, 0755); err != nil {
			return err
		}
		if err := moveExtractedContents(stage, dst); err != nil {
			return err
		}
		return applyDirectoryModes(dst, "", dirModes)
	}
	entry := filepath.Join(stage, filepath.FromSlash(archiveName))
	if err := os.Rename(entry, dst); err != nil {
		return fmt.Errorf("moving downloaded item to destination failed: %w", err)
	}
	return applyDirectoryModes(dst, filepath.FromSlash(archiveName), dirModes)
}

func tarPack(src string, w io.Writer) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}

	tw := tar.NewWriter(w)
	defer func() { _ = tw.Close() }()

	if !fi.IsDir() {
		header, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		header.Name = filepath.Base(src)
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = io.Copy(tw, f)
		return err
	}

	baseDir := filepath.Dir(src)
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(baseDir, path)
		if err != nil {
			return err
		}
		if relPath == "." {
			return nil
		}

		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}

		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}

		// Ensure tar paths always use forward slashes
		header.Name = filepath.ToSlash(relPath)
		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			_, err = io.Copy(tw, f)
			return err
		}

		return nil
	})
}

func tarPackAs(src, name string, w io.Writer) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return fmt.Errorf("cannot archive directory as a single file")
	}
	header, err := tar.FileInfoHeader(fi, "")
	if err != nil {
		return err
	}
	header.Name = filepath.Base(name)
	tw := tar.NewWriter(w)
	if err := tw.WriteHeader(header); err != nil {
		_ = tw.Close()
		return err
	}
	f, err := os.Open(src)
	if err != nil {
		_ = tw.Close()
		return err
	}
	if _, err := io.Copy(tw, f); err != nil {
		_ = f.Close()
		_ = tw.Close()
		return err
	}
	if err := f.Close(); err != nil {
		_ = tw.Close()
		return err
	}
	return tw.Close()
}

func tarUnpack(r io.Reader, dst string) error {
	_, _, err := tarUnpackWithInfo(r, dst)
	return err
}

func tarUnpackWithInfo(r io.Reader, dst string) (*tar.Header, map[string]os.FileMode, error) {
	tr := tar.NewReader(r)
	var first *tar.Header
	dirModes := make(map[string]os.FileMode)

	if err := os.MkdirAll(dst, 0755); err != nil {
		return nil, nil, err
	}
	root, err := filepath.Abs(dst)
	if err != nil {
		return nil, nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, nil, err
	}

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		if first == nil {
			copyHeader := *header
			first = &copyHeader
		}

		cleanPath := filepath.Clean(filepath.FromSlash(header.Name))
		if cleanPath == ".." || strings.HasPrefix(cleanPath, ".."+string(filepath.Separator)) || filepath.IsAbs(cleanPath) {
			return nil, nil, fmt.Errorf("insecure archive file path: %s", header.Name)
		}

		target := filepath.Join(root, cleanPath)
		rel, err := filepath.Rel(root, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, nil, fmt.Errorf("insecure archive target path: %s", target)
		}
		if header.Typeflag != tar.TypeDir && header.Typeflag != tar.TypeReg {
			return nil, nil, fmt.Errorf("unsupported archive entry type %d for %s", header.Typeflag, header.Name)
		}
		if cleanPath == "." {
			if header.Typeflag != tar.TypeDir {
				return nil, nil, fmt.Errorf("archive root entry must be a directory")
			}
			continue
		}
		if header.Typeflag == tar.TypeDir {
			dirModes[cleanPath] = header.FileInfo().Mode().Perm()
		}
		if err := writeArchiveEntry(root, cleanPath, header, tr); err != nil {
			return nil, nil, err
		}
	}

	_, err = io.Copy(io.Discard, r)
	return first, dirModes, err
}

func applyDirectoryModes(root, archiveRoot string, modes map[string]os.FileMode) error {
	paths := make([]string, 0, len(modes))
	for archivePath := range modes {
		paths = append(paths, archivePath)
	}
	sort.Slice(paths, func(i, j int) bool {
		return strings.Count(paths[i], string(filepath.Separator)) > strings.Count(paths[j], string(filepath.Separator))
	})
	for _, archivePath := range paths {
		var relative string
		if archiveRoot == "" {
			relative = archivePath
		} else if archivePath == archiveRoot {
			relative = "."
		} else if strings.HasPrefix(archivePath, archiveRoot+string(filepath.Separator)) {
			relative = strings.TrimPrefix(archivePath, archiveRoot+string(filepath.Separator))
		} else {
			continue
		}
		target := root
		if relative != "." {
			target = filepath.Join(root, relative)
		}
		info, err := os.Lstat(target)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("refusing to set archive directory mode on non-directory %s", target)
		}
		if err := os.Chmod(target, modes[archivePath]); err != nil {
			return err
		}
	}
	return nil
}

func moveExtractedContents(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		from := filepath.Join(src, entry.Name())
		to := filepath.Join(dst, entry.Name())
		info, err := os.Lstat(from)
		if err != nil {
			return err
		}
		if info.IsDir() {
			if err := os.MkdirAll(to, 0755); err != nil {
				return err
			}
			if err := moveExtractedContents(from, to); err != nil {
				return err
			}
			if err := os.Remove(from); err != nil {
				return err
			}
			continue
		}
		if err := os.Rename(from, to); err != nil {
			return fmt.Errorf("moving %s into destination failed: %w", entry.Name(), err)
		}
	}
	return nil
}
