package sbx

import "github.com/hoangndst/buddy-sandboxes/internal/gitrepo"

func validateGitCloneURL(raw string) error {
	return gitrepo.ValidateCredentialURL(raw)
}

func validateGitSubpath(value string) error {
	return gitrepo.ValidateSubpath(value)
}
