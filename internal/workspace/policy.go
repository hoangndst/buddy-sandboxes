package workspace

import (
	"fmt"
	"io/fs"
	"path"
	"runtime"
	"strings"
	"unicode/utf8"
)

var excludedComponents = map[string]struct{}{
	".git": {}, ".buddy": {}, ".dev": {}, ".agents": {}, ".buddy-sbx-recovery": {},
	"node_modules": {}, "target": {}, "dist": {}, ".next": {}, ".venv": {},
	"__pycache__": {}, ".pytest_cache": {}, ".mypy_cache": {}, ".tox": {},
	".ssh": {}, ".aws": {}, ".azure": {}, ".kube": {},
	"bin": {}, "build": {}, "out": {},
}

var excludedBasenames = map[string]struct{}{
	".npmrc": {}, ".pypirc": {},
}

// IsExcluded applies the same conservative path policy to preview, packing,
// validation, and result merging. Paths use slash separators.
func IsExcluded(name string) bool {
	parts := strings.Split(name, "/")
	for i, part := range parts {
		if _, ok := excludedComponents[part]; ok {
			return true
		}
		if part == ".config" && i+1 < len(parts) && parts[i+1] == "gcloud" {
			return true
		}
		if _, ok := excludedBasenames[part]; ok || strings.HasPrefix(part, ".env.") || part == ".env" {
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

func isExcludedForMerge(name string) bool {
	return isExcludedForFilesystem(name, runtime.GOOS == windowsOS || runtime.GOOS == "darwin")
}

func isExcludedForFilesystem(name string, caseInsensitive bool) bool {
	if IsExcluded(name) {
		return true
	}
	return caseInsensitive && IsExcluded(strings.ToLower(name))
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
