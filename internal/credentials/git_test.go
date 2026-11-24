package credentials

import "testing"

func TestCanReplaceGitClaimRequiresExactCheckedPriorUID(t *testing.T) {
	tests := []struct {
		name, currentUID, checkedUID string
		want                         bool
	}{
		{name: "same prior pod", currentUID: "pod-a", checkedUID: "pod-a", want: true},
		{name: "claim changed after liveness check", currentUID: "pod-b", checkedUID: "pod-a"},
		{name: "no prior pod checked", currentUID: "pod-a"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := canReplaceGitClaim(test.currentUID, test.checkedUID); got != test.want {
				t.Fatalf("canReplaceGitClaim(%q, %q) = %t, want %t", test.currentUID, test.checkedUID, got, test.want)
			}
		})
	}
}

func TestParseGitHubRepositoryRequiresExactCanonicalRepository(t *testing.T) {
	valid := []string{
		"https://github.com/acme/repo",
		"https://github.com/acme/repo.git",
	}
	for _, raw := range valid {
		repo, err := ParseGitHubRepository(raw)
		if err != nil || repo.Hostname() != "github.com" || repo.RepositoryPath() == "" || repo.CloneURL() != raw {
			t.Errorf("ParseGitHubRepository(%q) = %#v, %v", raw, repo, err)
		}
	}
	invalid := []string{
		"http://github.com/acme/repo",
		"https://user@github.com/acme/repo",
		"https://github.com/acme/repo/",
		"https://github.com/acme/repo/extra",
		"https://github.com/acme/repo?ref=main",
		"https://github.com.evil.example/acme/repo",
		"https://github.com:443/acme/repo",
	}
	for _, raw := range invalid {
		if _, err := ParseGitHubRepository(raw); err == nil {
			t.Errorf("ParseGitHubRepository(%q) unexpectedly succeeded", raw)
		}
	}
}

func TestValidGitTokenRejectsWhitespaceAndControls(t *testing.T) {
	for _, token := range []string{"", " token", "token ", "token\nvalue", "token\x00value"} {
		if validGitToken(token) {
			t.Errorf("validGitToken(%q) unexpectedly accepted token", token)
		}
	}
	if !validGitToken("github_pat_example") {
		t.Fatal("validGitToken rejected a well-formed opaque token")
	}
}
