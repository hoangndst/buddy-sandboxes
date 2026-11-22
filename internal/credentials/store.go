package credentials

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
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
	Service     string    `json:"service"`
	Scope       string    `json:"scope"`
	SandboxName string    `json:"sandboxName,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type Store struct {
	pool *pgxpool.Pool
	keys *KeySet
	now  func() time.Time
}

func NewStore(pool *pgxpool.Pool, keys *KeySet) *Store {
	return &Store{pool: pool, keys: keys, now: time.Now}
}

func (s *Store) Set(ctx context.Context, userID, service, scope, sandboxName, value string) error {
	if err := validateKey(value); err != nil {
		return err
	}
	if !validService(service) || (scope != ScopeUser && scope != ScopeSandbox) ||
		(scope == ScopeUser && sandboxName != "") || (scope == ScopeSandbox && sandboxName == "") ||
		len(value) > MaxCredentialLen {
		return fmt.Errorf("invalid provider credential")
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
		query = `INSERT INTO provider_credentials(id,user_id,service,scope,sandbox_name,ciphertext,key_version)
			VALUES($1,$2,$3,'user',NULL,$4,$5)
			ON CONFLICT(user_id,service) WHERE scope='user'
			DO UPDATE SET ciphertext=EXCLUDED.ciphertext,key_version=EXCLUDED.key_version,updated_at=now()`
		_, err = s.pool.Exec(ctx, query, id, userID, service, ciphertext, version)
	} else {
		query = `INSERT INTO provider_credentials(id,user_id,service,scope,sandbox_name,ciphertext,key_version)
			VALUES($1,$2,$3,'sandbox',$4,$5,$6)
			ON CONFLICT(user_id,service,sandbox_name) WHERE scope='sandbox'
			DO UPDATE SET ciphertext=EXCLUDED.ciphertext,key_version=EXCLUDED.key_version,updated_at=now()`
		_, err = s.pool.Exec(ctx, query, id, userID, service, sandboxName, ciphertext, version)
	}
	if err != nil {
		return fmt.Errorf("store provider credential")
	}
	return nil
}

func (s *Store) GetForSandbox(ctx context.Context, userID, service, sandboxName string) (string, error) {
	var ciphertext []byte
	var version int
	err := s.pool.QueryRow(ctx, `SELECT ciphertext,key_version FROM provider_credentials
		WHERE user_id=$1 AND service=$2 AND ((scope='sandbox' AND sandbox_name=$3) OR scope='user')
		ORDER BY CASE WHEN scope='sandbox' THEN 0 ELSE 1 END LIMIT 1`, userID, service, sandboxName).Scan(&ciphertext, &version)
	if err != nil {
		if err == pgx.ErrNoRows {
			return "", fmt.Errorf("provider credential is not configured")
		}
		return "", fmt.Errorf("resolve provider credential")
	}
	plaintext, err := s.keys.Decrypt(version, ciphertext)
	if err != nil {
		return "", fmt.Errorf("decrypt provider credential")
	}
	return string(plaintext), nil
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
	rows, err := s.pool.Query(ctx, `SELECT service,scope,COALESCE(sandbox_name,''),updated_at
		FROM provider_credentials WHERE user_id=$1 ORDER BY service,scope,sandbox_name`, userID)
	if err != nil {
		return nil, fmt.Errorf("list provider credentials")
	}
	defer rows.Close()
	items := make([]Metadata, 0)
	for rows.Next() {
		var item Metadata
		if err := rows.Scan(&item.Service, &item.Scope, &item.SandboxName, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("read provider credential metadata")
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read provider credential metadata")
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
