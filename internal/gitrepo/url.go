package gitrepo

import (
	"fmt"
	"net"
	"net/url"
	"path"
	"strings"
)

// ValidateCredentialURL accepts canonical HTTPS URLs suitable for a repository-bound Secret.
func ValidateCredentialURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Opaque != "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" ||
		u.RawPath != "" || u.EscapedPath() != u.Path || u.Port() != "" || u.Path == "" ||
		strings.Contains(u.Path, "//") || strings.HasSuffix(u.Path, "/") {
		return fmt.Errorf("Git clone URL must be a credential-free canonical HTTPS repository URL without query, fragment, or port")
	}
	host := u.Hostname()
	if strings.HasSuffix(host, ".") || strings.ToLower(host) != host || net.ParseIP(host) != nil || !validDNSHost(host) {
		return fmt.Errorf("Git clone URL host must be a lowercase DNS name")
	}
	for _, component := range strings.Split(strings.TrimPrefix(u.Path, "/"), "/") {
		if component == "" || component == "." || component == ".." {
			return fmt.Errorf("Git clone URL path must be canonical")
		}
	}
	return nil
}

func validDNSHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return false
			}
		}
	}
	return true
}

// ValidateSubpath ensures a workspace-relative repository destination stays below its mount.
func ValidateSubpath(value string) error {
	if value == "" {
		return nil
	}
	if strings.ContainsAny(value, "\\\x00") || path.IsAbs(value) || path.Clean(value) != value {
		return fmt.Errorf("Git subpath must be a clean relative path inside the workspace")
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." {
			return fmt.Errorf("Git subpath must be a clean relative path inside the workspace")
		}
	}
	return nil
}
