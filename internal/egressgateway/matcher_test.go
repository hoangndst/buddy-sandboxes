package egressgateway

import (
	"net"
	"testing"
)

func TestMatchDomain(t *testing.T) {
	tests := []struct {
		name           string
		host           string
		allowedDomains []string
		want           bool
	}{
		{
			name:           "empty allowed domains",
			host:           "github.com",
			allowedDomains: []string{},
			want:           false,
		},
		{
			name:           "exact match",
			host:           "github.com",
			allowedDomains: []string{"github.com"},
			want:           true,
		},
		{
			name:           "case insensitive match",
			host:           "GitHub.COM",
			allowedDomains: []string{"github.com"},
			want:           true,
		},
		{
			name:           "trailing dot stripped",
			host:           "github.com.",
			allowedDomains: []string{"github.com"},
			want:           true,
		},
		{
			name:           "with port stripped",
			host:           "github.com:443",
			allowedDomains: []string{"github.com"},
			want:           true,
		},
		{
			name:           "wildcard subdomain match",
			host:           "registry.npmjs.org",
			allowedDomains: []string{"*.npmjs.org"},
			want:           true,
		},
		{
			name:           "wildcard apex match",
			host:           "npmjs.org",
			allowedDomains: []string{"*.npmjs.org"},
			want:           true,
		},
		{
			name:           "wildcard mismatch root suffix",
			host:           "evilnpmjs.org",
			allowedDomains: []string{"*.npmjs.org"},
			want:           false,
		},
		{
			name:           "no match",
			host:           "attacker.com",
			allowedDomains: []string{"github.com", "*.npmjs.org"},
			want:           false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchDomain(tt.host, tt.allowedDomains)
			if got != tt.want {
				t.Errorf("MatchDomain(%q, %v) = %v, want %v", tt.host, tt.allowedDomains, got, tt.want)
			}
		})
	}
}

func TestValidatePublicIP(t *testing.T) {
	tests := []struct {
		name    string
		ip      net.IP
		wantErr bool
	}{
		{
			name:    "nil ip",
			ip:      nil,
			wantErr: true,
		},
		{
			name:    "unspecified ipv4 0.0.0.0",
			ip:      net.ParseIP("0.0.0.0"),
			wantErr: true,
		},
		{
			name:    "unspecified ipv6 ::",
			ip:      net.ParseIP("::"),
			wantErr: true,
		},
		{
			name:    "0.0.0.0/8 subnet",
			ip:      net.ParseIP("0.1.2.3"),
			wantErr: true,
		},
		{
			name:    "loopback 127.0.0.1",
			ip:      net.ParseIP("127.0.0.1"),
			wantErr: true,
		},
		{
			name:    "rfc1918 10.0.0.1",
			ip:      net.ParseIP("10.0.0.1"),
			wantErr: true,
		},
		{
			name:    "rfc1918 172.16.0.1",
			ip:      net.ParseIP("172.16.0.1"),
			wantErr: true,
		},
		{
			name:    "rfc1918 192.168.1.1",
			ip:      net.ParseIP("192.168.1.1"),
			wantErr: true,
		},
		{
			name:    "cloud metadata 169.254.169.254",
			ip:      net.ParseIP("169.254.169.254"),
			wantErr: true,
		},
		{
			name:    "cgnat 100.64.0.1",
			ip:      net.ParseIP("100.64.0.1"),
			wantErr: true,
		},
		{
			name:    "multicast 224.0.0.1",
			ip:      net.ParseIP("224.0.0.1"),
			wantErr: true,
		},
		{
			name:    "broadcast 255.255.255.255",
			ip:      net.ParseIP("255.255.255.255"),
			wantErr: true,
		},
		{
			name:    "valid public ip 104.18.2.161",
			ip:      net.ParseIP("104.18.2.161"),
			wantErr: false,
		},
		{
			name:    "valid public ip 140.82.121.4",
			ip:      net.ParseIP("140.82.121.4"),
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePublicIP(tt.ip)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidatePublicIP(%s) error = %v, wantErr %v", tt.ip, err, tt.wantErr)
			}
		})
	}
}
