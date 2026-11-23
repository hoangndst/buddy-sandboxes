package workspace

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"runtime"
	"sort"
	"strings"
	"unicode/utf8"

	ignore "github.com/sabhiram/go-gitignore"
)

// Platform security invariants that can never be bypassed or overridden by user ignore files.
var securityComponents = map[string]struct{}{
	".git": {}, ".buddy": {}, ".dev": {}, ".agents": {}, ".buddy-sbx-recovery": {},
	".ssh": {}, ".aws": {}, ".azure": {}, ".kube": {},
}

var securityBasenames = map[string]struct{}{
	".npmrc": {}, ".pypirc": {},
}

// Default build/artifact components excluded when no .gitignore is present.
var defaultBuildComponents = map[string]struct{}{
	"node_modules": {}, "target": {}, "dist": {}, ".next": {}, ".venv": {},
	"__pycache__": {}, ".pytest_cache": {}, ".mypy_cache": {}, ".tox": {},
	"bin": {}, "build": {}, "out": {},
}

// IsSecurityExcluded returns true if the path hits platform security boundaries
// (credentials, platform metadata, private keys, environment files).
func IsSecurityExcluded(name string) bool {
	parts := strings.Split(name, "/")
	for i, part := range parts {
		if _, ok := securityComponents[part]; ok {
			return true
		}
		if part == ".config" && i+1 < len(parts) && parts[i+1] == "gcloud" {
			return true
		}
		if _, ok := securityBasenames[part]; ok || strings.HasPrefix(part, ".env.") || part == ".env" {
			return true
		}
		if strings.HasSuffix(part, ".pem") || strings.HasSuffix(part, ".key") ||
			strings.HasSuffix(part, ".p12") || strings.HasSuffix(part, ".pfx") ||
			strings.HasPrefix(part, "id_rsa") || strings.HasPrefix(part, "id_ed25519") {
			return true
		}
	}
	return false
}

// isDefaultBuildExcluded checks common build and dependency output directories.
func isDefaultBuildExcluded(name string) bool {
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if _, ok := defaultBuildComponents[part]; ok {
			return true
		}
	}
	return false
}

// scopedIgnore encapsulates ignore rules defined at a specific directory prefix.
type scopedIgnore struct {
	prefix       string // relative directory path, e.g. "" for root or "packages/app"
	gitIgnore    *ignore.GitIgnore
	agentsIgnore *ignore.GitIgnore
}

// IgnoreEngine evaluates security invariants, .gitignore, and .agentsignore rules.
type IgnoreEngine struct {
	scopes []scopedIgnore
}

func (e *IgnoreEngine) addScope(prefix string, gitLines []string, agentLines []string) {
	scope := scopedIgnore{prefix: prefix}
	if len(gitLines) > 0 {
		scope.gitIgnore = ignore.CompileIgnoreLines(gitLines...)
	}
	if len(agentLines) > 0 {
		scope.agentsIgnore = ignore.CompileIgnoreLines(agentLines...)
	}
	e.scopes = append(e.scopes, scope)
	// Sort scopes by prefix depth/length descending so innermost directories take precedence
	sort.Slice(e.scopes, func(i, j int) bool {
		return len(e.scopes[i].prefix) > len(e.scopes[j].prefix)
	})
}

// AddScope registers ignore rules for a directory prefix (e.g. "packages/app").
func (e *IgnoreEngine) AddScope(prefix string, gitLines []string, agentLines []string) {
	e.addScope(prefix, gitLines, agentLines)
}

// NewIgnoreEngine loads .gitignore and .agentsignore from the project root and nested subdirectories.
// If an ignore file exists but cannot be read or scanned, an error is returned.
func NewIgnoreEngine(root *os.Root) (*IgnoreEngine, error) {
	engine := &IgnoreEngine{}
	if root == nil {
		return engine, nil
	}

	gitLines, err := readIgnoreFile(root, ".gitignore")
	if err != nil {
		return nil, err
	}
	agentLines, err := readIgnoreFile(root, ".agentsignore")
	if err != nil {
		return nil, err
	}
	if len(gitLines) > 0 || len(agentLines) > 0 {
		engine.addScope("", gitLines, agentLines)
	}

	err = fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == "." {
			return nil
		}
		if d.IsDir() {
			if IsSecurityExcluded(path) || isDefaultBuildExcluded(path) || engine.IsExcluded(path, true) {
				return fs.SkipDir
			}
			subGit, subErr := readIgnoreFile(root, path+"/.gitignore")
			if subErr != nil {
				return subErr
			}
			subAgent, subErr := readIgnoreFile(root, path+"/.agentsignore")
			if subErr != nil {
				return subErr
			}
			if len(subGit) > 0 || len(subAgent) > 0 {
				engine.addScope(path, subGit, subAgent)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return engine, nil
}

// NewIgnoreEngineFromLines compiles an IgnoreEngine from explicit in-memory rule sets.
func NewIgnoreEngineFromLines(gitLines []string, agentLines []string) *IgnoreEngine {
	engine := &IgnoreEngine{}
	if len(gitLines) > 0 || len(agentLines) > 0 {
		engine.addScope("", gitLines, agentLines)
	}
	return engine
}

func readIgnoreFile(root *os.Root, filename string) ([]string, error) {
	f, err := root.Open(filename)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open %s: %w", filename, err)
	}
	defer func() { _ = f.Close() }()

	var lines []string
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", filename, err)
	}
	return lines, nil
}

func matchPath(gi *ignore.GitIgnore, rel string, isDir bool) (bool, *ignore.IgnorePattern) {
	if gi == nil {
		return false, nil
	}
	matched, ip := gi.MatchesPathHow(rel)
	if matched {
		return true, ip
	}
	if isDir {
		matchedDir, ipDir := gi.MatchesPathHow(rel + "/")
		if matchedDir {
			return true, ipDir
		}
		if ipDir != nil && ipDir.Negate {
			return false, ipDir
		}
	}
	return matched, ip
}

// IsExcluded evaluates whether a path should be omitted from the workspace.
func (e *IgnoreEngine) IsExcluded(name string, isDir bool) bool {
	if IsSecurityExcluded(name) {
		return true
	}
	if e != nil {
		for _, scope := range e.scopes {
			var rel string
			if scope.prefix == "" {
				rel = name
			} else if name == scope.prefix {
				continue
			} else if strings.HasPrefix(name, scope.prefix+"/") {
				rel = strings.TrimPrefix(name, scope.prefix+"/")
			} else {
				continue
			}

			if scope.agentsIgnore != nil {
				matched, ip := matchPath(scope.agentsIgnore, rel, isDir)
				if matched {
					return true
				}
				if ip != nil && ip.Negate {
					return false
				}
			}

			if scope.gitIgnore != nil {
				matched, ip := matchPath(scope.gitIgnore, rel, isDir)
				if matched {
					return true
				}
				if ip != nil && ip.Negate {
					return false
				}
			}
		}
	}

	return isDefaultBuildExcluded(name)
}

// IsExcludedForFilesystem tests exclusion with optional case-folding.
func (e *IgnoreEngine) IsExcludedForFilesystem(name string, isDir bool, caseInsensitive bool) bool {
	if e.IsExcluded(name, isDir) {
		return true
	}
	return caseInsensitive && e.IsExcluded(strings.ToLower(name), isDir)
}

var defaultIgnoreEngine = &IgnoreEngine{}

// IsExcluded applies the baseline security and default build exclusion policy.
// Retained for backward compatibility with static checks.
func IsExcluded(name string) bool {
	return defaultIgnoreEngine.IsExcluded(name, false)
}

func isExcludedForFilesystem(name string, caseInsensitive bool) bool {
	return defaultIgnoreEngine.IsExcludedForFilesystem(name, false, caseInsensitive)
}

// ValidatePath validates an archive-relative path before it is used with an
// os.Root. The archive root itself is represented by an empty name only when
// allowRoot is true.
func ValidatePath(name string, limits Limits, allowRoot bool) (string, error) {
	if name == "" {
		if allowRoot {
			return ".", nil
		}
		return "", fmt.Errorf("empty workspace path")
	}
	if len(name) > limits.MaxPathBytes {
		return "", fmt.Errorf("workspace path exceeds %d bytes", limits.MaxPathBytes)
	}
	if !utf8.ValidString(name) || strings.ContainsRune(name, '\x00') || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") ||
		strings.ContainsAny(name, `<>:"|?*`) {
		return "", fmt.Errorf("invalid workspace path")
	}
	if len(name) >= 2 && ((name[0] >= 'A' && name[0] <= 'Z') || (name[0] >= 'a' && name[0] <= 'z')) && name[1] == ':' {
		return "", fmt.Errorf("volume paths are not allowed")
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != name {
		return "", fmt.Errorf("workspace path is not a clean relative path")
	}
	for _, component := range strings.Split(name, "/") {
		if component == "" || component == "." || component == ".." ||
			strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") || isWindowsDeviceName(component) {
			return "", fmt.Errorf("invalid workspace path component")
		}
		for _, r := range component {
			if r < 0x20 || r == 0x7f {
				return "", fmt.Errorf("workspace path contains a control character")
			}
		}
	}
	return name, nil
}

func isWindowsDeviceName(component string) bool {
	name := component
	if dot := strings.IndexByte(name, '.'); dot >= 0 {
		name = name[:dot]
	}
	name = strings.ToUpper(strings.TrimRight(name, " ."))
	switch name {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if strings.HasPrefix(name, "COM") || strings.HasPrefix(name, "LPT") {
		suffix := strings.TrimPrefix(strings.TrimPrefix(name, "COM"), "LPT")
		return suffix == "1" || suffix == "2" || suffix == "3" || suffix == "4" || suffix == "5" ||
			suffix == "6" || suffix == "7" || suffix == "8" || suffix == "9" || suffix == "¹" || suffix == "²" || suffix == "³"
	}
	return false
}

func normalizeMode(mode fs.FileMode, directory bool) fs.FileMode {
	if runtime.GOOS != windowsOS {
		return mode.Perm() & 0o777
	}
	if directory {
		return 0o755
	}
	if mode&0o200 != 0 {
		return 0o644
	}
	return 0o444
}

func hasSpecialMode(mode fs.FileMode) bool {
	return mode&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0
}
