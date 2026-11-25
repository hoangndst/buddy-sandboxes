package sbx

import "testing"

func TestValidateGitCloneURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
	}{
		{name: "canonical HTTPS", url: "https://github.com/example/repo.git", want: true},
		{name: "credential userinfo", url: "https://user:token@github.com/example/repo.git"},
		{name: "HTTP", url: "http://github.com/example/repo.git"},
		{name: "file scheme", url: "file:///tmp/repo"},
		{name: "query", url: "https://github.com/example/repo.git?token=x"},
		{name: "fragment", url: "https://github.com/example/repo.git#main"},
		{name: "encoded path", url: "https://github.com/example%2Frepo.git"},
		{name: "explicit port", url: "https://github.com:443/example/repo.git"},
		{name: "uppercase host", url: "https://GitHub.com/example/repo.git"},
		{name: "IP literal host", url: "https://192.0.2.1/example/repo.git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateGitCloneURL(tt.url)
			if (err == nil) != tt.want {
				t.Fatalf("validateGitCloneURL(%q) error=%v, wantValid=%t", tt.url, err, tt.want)
			}
		})
	}
}

func TestValidateGitSubpath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: "", want: true},
		{path: "service/api", want: true},
		{path: "../outside"},
		{path: "service/../outside"},
		{path: "/absolute"},
		{path: "service\\api"},
		{path: "service//api"},
		{path: "./service"},
	}
	for _, tt := range tests {
		err := validateGitSubpath(tt.path)
		if (err == nil) != tt.want {
			t.Errorf("validateGitSubpath(%q) error=%v, wantValid=%t", tt.path, err, tt.want)
		}
	}
}
