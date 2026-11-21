package egressgateway

import (
	"testing"
	"time"
)

func TestTokenLifecycle(t *testing.T) {
	secret := []byte("super-secret-hmac-key-1234567890123456")
	otherSecret := []byte("different-secret-key-0987654321098765")

	claims := Claims{
		Sub:     "sb-test-01",
		Ns:      "default",
		Domains: []string{"github.com", "*.npmjs.org"},
		Exp:     time.Now().Add(1 * time.Hour).Unix(),
	}

	token, err := GenerateToken(claims, secret)
	if err != nil {
		t.Fatalf("GenerateToken failed: %v", err)
	}
	if token == "" {
		t.Fatal("expected non-empty token")
	}

	verified, err := VerifyToken(token, secret)
	if err != nil {
		t.Fatalf("VerifyToken failed: %v", err)
	}
	if verified.Sub != claims.Sub || verified.Ns != claims.Ns {
		t.Fatalf("claims mismatch: got %+v, want %+v", verified, claims)
	}
	if len(verified.Domains) != 2 || verified.Domains[0] != "github.com" {
		t.Fatalf("domains mismatch: got %v", verified.Domains)
	}

	// Verify failure with wrong secret
	_, err = VerifyToken(token, otherSecret)
	if err == nil {
		t.Fatal("expected signature verification failure with wrong secret")
	}

	// Verify expiration failure
	expiredClaims := Claims{
		Sub:     "sb-expired",
		Ns:      "default",
		Domains: []string{"github.com"},
		Exp:     time.Now().Add(-1 * time.Minute).Unix(),
	}
	expiredToken, err := GenerateToken(expiredClaims, secret)
	if err != nil {
		t.Fatalf("GenerateToken failed: %v", err)
	}
	_, err = VerifyToken(expiredToken, secret)
	if err == nil || err != ErrTokenExpired {
		t.Fatalf("expected ErrTokenExpired, got %v", err)
	}
}
