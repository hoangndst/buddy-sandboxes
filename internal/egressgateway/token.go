package egressgateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrInvalidTokenFormat = errors.New("invalid token format")
	ErrInvalidSignature   = errors.New("invalid token signature")
	ErrTokenExpired       = errors.New("token expired")
	ErrEmptySecret        = errors.New("hmac secret cannot be empty")
)

// Claims represents the authorization payload minted by the controller.
type Claims struct {
	Sub     string   `json:"sub"`
	Ns      string   `json:"ns"`
	Domains []string `json:"domains"`
	Exp     int64    `json:"exp"`
}

// GenerateToken creates an HMAC-SHA256 signed bearer token.
func GenerateToken(claims Claims, secret []byte) (string, error) {
	if len(secret) == 0 {
		return "", ErrEmptySecret
	}
	payloadBytes, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("failed to marshal token claims: %w", err)
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payloadBytes)

	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(encodedPayload))
	sig := mac.Sum(nil)
	encodedSig := base64.RawURLEncoding.EncodeToString(sig)

	return encodedPayload + "." + encodedSig, nil
}

// VerifyToken verifies the HMAC signature and expiration timestamp of a token.
func VerifyToken(tokenStr string, secret []byte) (*Claims, error) {
	if len(secret) == 0 {
		return nil, ErrEmptySecret
	}
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 2 {
		return nil, ErrInvalidTokenFormat
	}
	encodedPayload := parts[0]
	encodedSig := parts[1]

	sig, err := base64.RawURLEncoding.DecodeString(encodedSig)
	if err != nil {
		return nil, ErrInvalidTokenFormat
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(encodedPayload))
	expectedSig := mac.Sum(nil)

	if subtle.ConstantTimeCompare(sig, expectedSig) != 1 {
		return nil, ErrInvalidSignature
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		return nil, ErrInvalidTokenFormat
	}

	var claims Claims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil, fmt.Errorf("failed to unmarshal token claims: %w", err)
	}

	if claims.Exp > 0 && time.Now().Unix() > claims.Exp {
		return nil, ErrTokenExpired
	}

	return &claims, nil
}
