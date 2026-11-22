package credentials

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	ServiceOpenAI    = "openai"
	ServiceAnthropic = "anthropic"
	ScopeUser        = "user"
	ScopeSandbox     = "sandbox"
	MaxCredentialLen = 8 << 10
)

type Metadata struct {
	Service      string    `json:"service"`
	Scope        string    `json:"scope"`
	SandboxName  string    `json:"sandboxName,omitempty"`
	BaseURL      string    `json:"baseUrl,omitempty"`
	DefaultModel string    `json:"defaultModel,omitempty"`
	AuthType     string    `json:"authType,omitempty"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type Options struct {
	BaseURL      string
	DefaultModel string
	AuthType     string
}

type Record struct {
	Key          string
	BaseURL      string
	DefaultModel string
	AuthType     string
}

type Store struct {
	pool *pgxpool.Pool
	keys *KeySet
	now  func() time.Time
}

func NewStore(pool *pgxpool.Pool, keys *KeySet) *Store {
	return &Store{pool: pool, keys: keys, now: time.Now}
}

func (s *Store) Set(ctx context.Context, userID, service, scope, sandboxName, value string, opts ...Options) error {
	if err := validateKey(value); err != nil {
		return err
	}
	if !validService(service) || (scope != ScopeUser && scope != ScopeSandbox) ||
		(scope == ScopeUser && sandboxName != "") || (scope == ScopeSandbox && sandboxName == "") ||
		len(value) > MaxCredentialLen {
		return fmt.Errorf("invalid provider credential")
	}
	var opt Options
	if len(opts) > 0 {
		opt = opts[0]
	}
	if err := ValidateBaseURL(opt.BaseURL); err != nil {
		return err
	}
	if err := ValidateModelName(opt.DefaultModel); err != nil {
		return err
	}
	ciphertext, version, err := s.keys.Encrypt([]byte(value))
	if err != nil {
		return fmt.Errorf("encrypt provider credential: %w", err)
	}
	id, err := randomCredentialID()
	if err != nil {
		return err
	}
	var query string
	if scope == ScopeUser {
		query = `INSERT INTO provider_credentials(id,user_id,service,scope,sandbox_name,ciphertext,key_version,base_url,default_model,auth_type)
			VALUES($1,$2,$3,'user',NULL,$4,$5,$6,$7,$8)
			ON CONFLICT(user_id,service) WHERE scope='user'
			DO UPDATE SET ciphertext=EXCLUDED.ciphertext,key_version=EXCLUDED.key_version,
				base_url=COALESCE(EXCLUDED.base_url,provider_credentials.base_url),
				default_model=COALESCE(EXCLUDED.default_model,provider_credentials.default_model),
				auth_type=COALESCE(EXCLUDED.auth_type,provider_credentials.auth_type),
				updated_at=now()`
		_, err = s.pool.Exec(ctx, query, id, userID, service, ciphertext, version, nullIfEmpty(opt.BaseURL), nullIfEmpty(opt.DefaultModel), nullIfEmpty(opt.AuthType))
	} else {
		query = `INSERT INTO provider_credentials(id,user_id,service,scope,sandbox_name,ciphertext,key_version,base_url,default_model,auth_type)
			VALUES($1,$2,$3,'sandbox',$4,$5,$6,$7,$8,$9)
			ON CONFLICT(user_id,service,sandbox_name) WHERE scope='sandbox'
			DO UPDATE SET ciphertext=EXCLUDED.ciphertext,key_version=EXCLUDED.key_version,
				base_url=COALESCE(EXCLUDED.base_url,provider_credentials.base_url),
				default_model=COALESCE(EXCLUDED.default_model,provider_credentials.default_model),
				auth_type=COALESCE(EXCLUDED.auth_type,provider_credentials.auth_type),
				updated_at=now()`
		_, err = s.pool.Exec(ctx, query, id, userID, service, sandboxName, ciphertext, version, nullIfEmpty(opt.BaseURL), nullIfEmpty(opt.DefaultModel), nullIfEmpty(opt.AuthType))
	}
	if err != nil {
		return fmt.Errorf("store provider credential: %w", err)
	}
	return nil
}

func (s *Store) GetRecordForSandbox(ctx context.Context, userID, service, sandboxName string) (Record, error) {
	var ciphertext []byte
	var version int
	var baseURL, defaultModel, authType *string
	err := s.pool.QueryRow(ctx, `SELECT ciphertext,key_version,base_url,default_model,auth_type FROM provider_credentials
		WHERE user_id=$1 AND service=$2 AND ((scope='sandbox' AND sandbox_name=$3) OR scope='user')
		ORDER BY CASE WHEN scope='sandbox' THEN 0 ELSE 1 END LIMIT 1`, userID, service, sandboxName).Scan(&ciphertext, &version, &baseURL, &defaultModel, &authType)
	if err != nil {
		if err == pgx.ErrNoRows {
			return Record{}, fmt.Errorf("provider credential is not configured")
		}
		return Record{}, fmt.Errorf("resolve provider credential")
	}
	plaintext, err := s.keys.Decrypt(version, ciphertext)
	if err != nil {
		return Record{}, fmt.Errorf("decrypt provider credential")
	}
	var bURL, defModel, aType string
	if baseURL != nil {
		bURL = *baseURL
	}
	if defaultModel != nil {
		defModel = *defaultModel
	}
	if authType != nil {
		aType = *authType
	}
	return Record{
		Key:          string(plaintext),
		BaseURL:      bURL,
		DefaultModel: defModel,
		AuthType:     aType,
	}, nil
}

func (s *Store) GetForSandbox(ctx context.Context, userID, service, sandboxName string) (string, error) {
	rec, err := s.GetRecordForSandbox(ctx, userID, service, sandboxName)
	if err != nil {
		return "", err
	}
	return rec.Key, nil
}

func (s *Store) HasForSandbox(ctx context.Context, userID, service, sandboxName string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM provider_credentials
		WHERE user_id=$1 AND service=$2 AND ((scope='sandbox' AND sandbox_name=$3) OR scope='user'))`, userID, service, sandboxName).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check provider credential")
	}
	return exists, nil
}

func (s *Store) List(ctx context.Context, userID string) ([]Metadata, error) {
	rows, err := s.pool.Query(ctx, `SELECT service,scope,COALESCE(sandbox_name,''),COALESCE(base_url,''),COALESCE(default_model,''),COALESCE(auth_type,''),updated_at
		FROM provider_credentials WHERE user_id=$1 ORDER BY service,scope,sandbox_name`, userID)
	if err != nil {
		return nil, fmt.Errorf("list provider credentials")
	}
	defer rows.Close()
	items := make([]Metadata, 0)
	for rows.Next() {
		var item Metadata
		if err := rows.Scan(&item.Service, &item.Scope, &item.SandboxName, &item.BaseURL, &item.DefaultModel, &item.AuthType, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("read provider credential metadata: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read provider credential metadata: %w", err)
	}
	return items, nil
}

func (s *Store) Delete(ctx context.Context, userID, service, scope, sandboxName string) error {
	if !validService(service) || (scope != ScopeUser && scope != ScopeSandbox) ||
		(scope == ScopeUser && sandboxName != "") || (scope == ScopeSandbox && sandboxName == "") {
		return fmt.Errorf("invalid provider credential scope")
	}
	var err error
	if scope == ScopeUser {
		_, err = s.pool.Exec(ctx, `DELETE FROM provider_credentials WHERE user_id=$1 AND service=$2 AND scope='user'`, userID, service)
	} else {
		_, err = s.pool.Exec(ctx, `DELETE FROM provider_credentials WHERE user_id=$1 AND service=$2 AND scope='sandbox' AND sandbox_name=$3`, userID, service, sandboxName)
	}
	if err != nil {
		return fmt.Errorf("remove provider credential")
	}
	return nil
}

func validService(service string) bool {
	return service == ServiceOpenAI || service == ServiceAnthropic
}

func randomCredentialID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("generate credential ID: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func validateKey(value string) error {
	if strings.TrimSpace(value) != value || len(value) == 0 || len(value) > MaxCredentialLen || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("invalid provider credential")
	}
	return nil
}

func nullIfEmpty(s string) *string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// ValidateBaseURL checks that custom base URLs are valid absolute HTTPS URLs with valid FQDNs.
func ValidateBaseURL(rawURL string) error {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil
	}
	u, err := url.ParseRequestURI(rawURL)
	if err != nil {
		return fmt.Errorf("invalid provider URL: %w", err)
	}
	if u.Scheme != "https" {
		// Allow plain http only for localhost/127.0.0.1 in testing/dev environments
		if u.Scheme != "http" || (u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1") {
			return fmt.Errorf("provider URL must use HTTPS scheme")
		}
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("provider URL must include a valid host")
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return fmt.Errorf("provider URL host must be an FQDN, not a raw IP literal")
		}
	}
	if port := u.Port(); port != "" && port != "443" {
		if u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" {
			return fmt.Errorf("provider URL port %s is not permitted; egress proxy only permits standard HTTPS port 443", port)
		}
	}
	hostLower := strings.ToLower(host)
	blockedSuffixes := []string{".cluster.local", ".svc", ".internal", ".local", ".onion", ".test", ".example", ".invalid"}
	for _, suffix := range blockedSuffixes {
		if strings.HasSuffix(hostLower, suffix) {
			return fmt.Errorf("provider URL host %q is not permitted", host)
		}
	}
	return nil
}

var modelNameRegex = regexp.MustCompile(`^[a-zA-Z0-9_\-\.:/]{1,128}$`)

// ValidateModelName validates model identifiers according to DEC-040.
func ValidateModelName(model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	if !modelNameRegex.MatchString(model) || strings.Contains(model, "..") || strings.HasPrefix(model, "/") || strings.HasSuffix(model, "/") {
		return fmt.Errorf("invalid model identifier %q", model)
	}
	return nil
}
