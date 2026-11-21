package main

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

type copyEndpoint struct {
	isRemote bool
	sandbox  string
	path     string
}

func parseCopyEndpoint(arg string) copyEndpoint {
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
				if _, err := os.Stat(absSrc); err != nil {
					return fmt.Errorf("source path %s does not exist: %w", absSrc, err)
				}

				pr, pw := io.Pipe()
				go func() {
					err := tarPack(absSrc, pw)
					_ = pw.CloseWithError(err)
				}()

				if err := c.Upload(ctx, dst.sandbox, dst.path, pr); err != nil {
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

			parent := filepath.Dir(absDst)
			if err := os.MkdirAll(parent, 0755); err != nil {
				return fmt.Errorf("creating destination parent failed: %w", err)
			}
			stage, err := os.MkdirTemp(parent, ".buddy-sbx-cp-*")
			if err != nil {
				return fmt.Errorf("creating temporary destination failed: %w", err)
			}
			defer func() { _ = os.RemoveAll(stage) }()

			first, err := tarUnpackWithInfo(rc, stage)
			if err != nil {
				return fmt.Errorf("extracting archive failed: %w", err)
			}
			if first == nil {
				return fmt.Errorf("download archive is empty")
			}
			destinationIsDirectory := strings.HasSuffix(args[1], string(os.PathSeparator))
			if dstInfo, statErr := os.Stat(absDst); statErr == nil && dstInfo.IsDir() {
				destinationIsDirectory = true
			}
			if destinationIsDirectory {
				if err := os.MkdirAll(absDst, 0755); err != nil {
					return err
				}
				if err := moveExtractedContents(stage, absDst); err != nil {
					return err
				}
			} else {
				entry := filepath.Join(stage, filepath.FromSlash(first.Name))
				if err := os.Rename(entry, absDst); err != nil {
					return fmt.Errorf("moving downloaded item to destination failed: %w", err)
				}
			}

			fmt.Printf("Successfully copied %s:%s to %s\n", src.sandbox, src.path, args[1])
			return nil
		},
	}

	return cmd
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

func tarUnpack(r io.Reader, dst string) error {
	_, err := tarUnpackWithInfo(r, dst)
	return err
}

func tarUnpackWithInfo(r io.Reader, dst string) (*tar.Header, error) {
	tr := tar.NewReader(r)
	var first *tar.Header

	if err := os.MkdirAll(dst, 0755); err != nil {
		return nil, err
	}
	root, err := filepath.Abs(dst)
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if first == nil {
			copyHeader := *header
			first = &copyHeader
		}

		cleanPath := filepath.Clean(filepath.FromSlash(header.Name))
		if cleanPath == ".." || strings.HasPrefix(cleanPath, ".."+string(filepath.Separator)) || filepath.IsAbs(cleanPath) {
			return nil, fmt.Errorf("insecure archive file path: %s", header.Name)
		}

		target := filepath.Join(root, cleanPath)
		rel, err := filepath.Rel(root, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("insecure archive target path: %s", target)
		}
		if err := rejectSymlinkComponents(root, filepath.Dir(target)); err != nil {
			return nil, err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := rejectSymlinkComponents(root, target); err != nil {
				return nil, err
			}
			if err := os.MkdirAll(target, 0755); err != nil {
				return nil, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return nil, err
			}
			if err := rejectSymlinkComponents(root, target); err != nil {
				return nil, err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR|os.O_TRUNC, header.FileInfo().Mode().Perm())
			if err != nil {
				return nil, err
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return nil, err
			}
			_ = f.Close()
		default:
			return nil, fmt.Errorf("unsupported archive entry type %d for %s", header.Typeflag, header.Name)
		}
	}

	_, err = io.Copy(io.Discard, r)
	return first, err
}

func moveExtractedContents(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		from := filepath.Join(src, entry.Name())
		to := filepath.Join(dst, entry.Name())
		if err := os.Rename(from, to); err != nil {
			return fmt.Errorf("moving %s into destination failed: %w", entry.Name(), err)
		}
	}
	return nil
}

func rejectSymlinkComponents(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("insecure archive target path: %s", target)
	}
	current := root
	if rel == "." {
		return nil
	}
	for _, component := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive path traverses symlink: %s", current)
		}
	}
	return nil
}
