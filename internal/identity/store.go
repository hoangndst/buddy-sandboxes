package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const LoginTokenLifetime = 90 * 24 * time.Hour

var ErrUnauthorized = errors.New("unauthorized")

type Role string

const (
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
)

type Principal struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Namespace   string `json:"namespace"`
	Role        Role   `json:"role"`
}

type User struct {
	Principal
	Disabled bool      `json:"disabled"`
	Created  time.Time `json:"created"`
}

type IssuedToken struct {
	ID        string
	Value     string
	ExpiresAt time.Time
}

type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, now: time.Now}
}

func (s *Store) Authenticate(ctx context.Context, token string) (Principal, error) {
	if token == "" {
		return Principal{}, ErrUnauthorized
	}
	hash := hashToken(token)
	var principal Principal
	err := s.pool.QueryRow(ctx, `SELECT u.id, u.display_name, COALESCE(u.namespace, ''), u.role
		FROM auth_tokens t JOIN users u ON u.id=t.user_id
		WHERE t.token_hash=$1 AND t.revoked_at IS NULL AND t.expires_at > now() AND u.disabled=false`, hash).
		Scan(&principal.ID, &principal.DisplayName, &principal.Namespace, &principal.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrUnauthorized
	}
	if err != nil {
		return Principal{}, fmt.Errorf("authenticate Buddy token: %w", err)
	}
	return principal, nil
}

func (s *Store) EnableNamespace(ctx context.Context, namespace string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO namespace_registry(name) VALUES($1) ON CONFLICT(name) DO NOTHING`, namespace)
	if err != nil {
		return fmt.Errorf("register enabled namespace: %w", err)
	}
	return nil
}

func (s *Store) CreateUser(ctx context.Context, displayName, namespace string) (User, error) {
	id, err := randomID()
	if err != nil {
		return User{}, err
	}
	var user User
	err = s.pool.QueryRow(ctx, `INSERT INTO users(id, display_name, role, namespace)
		VALUES($1,$2,'user',$3) RETURNING id, display_name, namespace, role, disabled, created_at`,
		id, displayName, namespace).Scan(&user.ID, &user.DisplayName, &user.Namespace, &user.Role, &user.Disabled, &user.Created)
	if err != nil {
		return User{}, fmt.Errorf("create user: %w", err)
	}
	return user, nil
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, display_name, COALESCE(namespace,''), role, disabled, created_at FROM users ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	users := make([]User, 0)
	for rows.Next() {
		var user User
		if err := rows.Scan(&user.ID, &user.DisplayName, &user.Namespace, &user.Role, &user.Disabled, &user.Created); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read users: %w", err)
	}
	return users, nil
}

func (s *Store) GetUser(ctx context.Context, userID string) (User, error) {
	var user User
	err := s.pool.QueryRow(ctx, `SELECT id,display_name,COALESCE(namespace,''),role,disabled,created_at FROM users WHERE id=$1`, userID).
		Scan(&user.ID, &user.DisplayName, &user.Namespace, &user.Role, &user.Disabled, &user.Created)
	if err != nil {
		return User{}, fmt.Errorf("get user")
	}
	return user, nil
}

func (s *Store) NamespaceEnabled(ctx context.Context, namespace string) (bool, error) {
	var enabled bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM namespace_registry WHERE name=$1)`, namespace).Scan(&enabled); err != nil {
		return false, fmt.Errorf("check namespace registration")
	}
	return enabled, nil
}

func (s *Store) ListEnabledNamespaces(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT name FROM namespace_registry ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list enabled namespaces")
	}
	defer rows.Close()
	namespaces := make([]string, 0)
	for rows.Next() {
		var namespace string
		if err := rows.Scan(&namespace); err != nil {
			return nil, fmt.Errorf("read enabled namespaces")
		}
		namespaces = append(namespaces, namespace)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read enabled namespaces")
	}
	return namespaces, nil
}

func (s *Store) Audit(ctx context.Context, actorID, action, target string, succeeded bool) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO audit_events(actor_user_id,action,target,succeeded) VALUES($1,$2,$3,$4)`, actorID, action, target, succeeded)
	if err != nil {
		return fmt.Errorf("record audit event")
	}
	return nil
}

func (s *Store) IssueToken(ctx context.Context, userID string) (IssuedToken, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return IssuedToken{}, fmt.Errorf("begin token issuance: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var disabled bool
	if err := tx.QueryRow(ctx, `SELECT disabled FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&disabled); err != nil {
		return IssuedToken{}, fmt.Errorf("find user for token issuance: %w", err)
	}
	if disabled {
		return IssuedToken{}, fmt.Errorf("cannot issue a token for a disabled user")
	}
	issued, hash, err := newToken()
	if err != nil {
		return IssuedToken{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO auth_tokens(id,user_id,token_hash,expires_at) VALUES($1,$2,$3,$4)`,
		issued.ID, userID, hash, issued.ExpiresAt); err != nil {
		return IssuedToken{}, fmt.Errorf("persist Buddy token: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return IssuedToken{}, fmt.Errorf("commit Buddy token: %w", err)
	}
	return issued, nil
}

func (s *Store) RevokeToken(ctx context.Context, userID, tokenID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE auth_tokens SET revoked_at=now() WHERE user_id=$1 AND id=$2 AND revoked_at IS NULL`, userID, tokenID)
	if err != nil {
		return fmt.Errorf("revoke Buddy token: %w", err)
	}
	return nil
}

func (s *Store) SetDisabled(ctx context.Context, userID string, disabled bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin user status update: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(485017234)`); err != nil {
		return fmt.Errorf("lock administrator state: %w", err)
	}
	var role Role
	if err := tx.QueryRow(ctx, `SELECT role FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&role); err != nil {
		return fmt.Errorf("find user: %w", err)
	}
	if role == RoleAdmin && disabled {
		var activeAdmins int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM users WHERE role='admin' AND disabled=false AND id<>$1`, userID).Scan(&activeAdmins); err != nil {
			return fmt.Errorf("count active administrators: %w", err)
		}
		if activeAdmins == 0 {
			return fmt.Errorf("cannot disable the last active administrator")
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET disabled=$2 WHERE id=$1`, userID, disabled); err != nil {
		return fmt.Errorf("update user status: %w", err)
	}
	if disabled {
		if _, err := tx.Exec(ctx, `UPDATE auth_tokens SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL`, userID); err != nil {
			return fmt.Errorf("revoke disabled user's tokens: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE broker_capabilities SET state='revoked', revoked_at=now() WHERE user_id=$1 AND state<>'revoked'`, userID); err != nil {
			return fmt.Errorf("revoke disabled user's broker capabilities: %w", err)
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) SetRole(ctx context.Context, userID string, role Role) error {
	if role != RoleAdmin && role != RoleUser {
		return fmt.Errorf("invalid user role")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin role update: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(485017234)`); err != nil {
		return fmt.Errorf("lock administrator state: %w", err)
	}
	var current Role
	var disabled bool
	if err := tx.QueryRow(ctx, `SELECT role, disabled FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&current, &disabled); err != nil {
		return fmt.Errorf("find user: %w", err)
	}
	if current == RoleAdmin && role != RoleAdmin && !disabled {
		var activeAdmins int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM users WHERE role='admin' AND disabled=false AND id<>$1`, userID).Scan(&activeAdmins); err != nil {
			return fmt.Errorf("count active administrators: %w", err)
		}
		if activeAdmins == 0 {
			return fmt.Errorf("cannot demote the last active administrator")
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET role=$2 WHERE id=$1`, userID, role); err != nil {
		return fmt.Errorf("update user role: %w", err)
	}
	return tx.Commit(ctx)
}

func (s *Store) BootstrapAdmin(ctx context.Context, bootstrapToken, displayName, namespace string) (User, IssuedToken, error) {
	if bootstrapToken == "" {
		return User{}, IssuedToken{}, ErrUnauthorized
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, IssuedToken{}, fmt.Errorf("begin administrator bootstrap: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var savedHash []byte
	var consumed *time.Time
	if err := tx.QueryRow(ctx, `SELECT token_hash, consumed_at FROM bootstrap_state WHERE id=true FOR UPDATE`).Scan(&savedHash, &consumed); err != nil {
		return User{}, IssuedToken{}, fmt.Errorf("read administrator bootstrap state: %w", err)
	}
	providedHash := hashToken(bootstrapToken)
	if consumed != nil || len(savedHash) != len(providedHash) || subtle.ConstantTimeCompare(savedHash, providedHash) != 1 {
		return User{}, IssuedToken{}, ErrUnauthorized
	}
	var adminCount int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM users WHERE role='admin'`).Scan(&adminCount); err != nil {
		return User{}, IssuedToken{}, fmt.Errorf("count administrators: %w", err)
	}
	if adminCount != 0 {
		return User{}, IssuedToken{}, fmt.Errorf("administrator bootstrap has already been completed")
	}
	id, err := randomID()
	if err != nil {
		return User{}, IssuedToken{}, err
	}
	var user User
	if err := tx.QueryRow(ctx, `INSERT INTO users(id,display_name,role,namespace) VALUES($1,$2,'admin',$3)
		RETURNING id,display_name,COALESCE(namespace,''),role,disabled,created_at`, id, displayName, namespace).
		Scan(&user.ID, &user.DisplayName, &user.Namespace, &user.Role, &user.Disabled, &user.Created); err != nil {
		return User{}, IssuedToken{}, fmt.Errorf("create first administrator: %w", err)
	}
	issued, tokenHash, err := newToken()
	if err != nil {
		return User{}, IssuedToken{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO auth_tokens(id,user_id,token_hash,expires_at) VALUES($1,$2,$3,$4)`, issued.ID, user.ID, tokenHash, issued.ExpiresAt); err != nil {
		return User{}, IssuedToken{}, fmt.Errorf("issue administrator login token: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE bootstrap_state SET token_hash=NULL,consumed_at=now() WHERE id=true`); err != nil {
		return User{}, IssuedToken{}, fmt.Errorf("consume administrator bootstrap: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit_events(actor_user_id,action,target,succeeded) VALUES($1,'bootstrap_admin',$2,true)`, user.ID, user.ID); err != nil {
		return User{}, IssuedToken{}, fmt.Errorf("audit administrator bootstrap: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, IssuedToken{}, fmt.Errorf("commit administrator bootstrap: %w", err)
	}
	return user, issued, nil
}

func (s *Store) ImportLegacyAdmin(ctx context.Context, displayName, namespace, legacyToken string) (User, IssuedToken, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, IssuedToken{}, fmt.Errorf("begin legacy identity migration: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var enabled bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM namespace_registry WHERE name=$1)`, namespace).Scan(&enabled); err != nil {
		return User{}, IssuedToken{}, fmt.Errorf("check legacy namespace: %w", err)
	}
	if !enabled {
		return User{}, IssuedToken{}, fmt.Errorf("legacy namespace is not enabled")
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&count); err != nil {
		return User{}, IssuedToken{}, fmt.Errorf("check existing users: %w", err)
	}
	if count != 0 {
		return User{}, IssuedToken{}, fmt.Errorf("legacy migration requires an empty identity store")
	}
	id, err := randomID()
	if err != nil {
		return User{}, IssuedToken{}, err
	}
	var user User
	if err := tx.QueryRow(ctx, `INSERT INTO users(id,display_name,role,namespace) VALUES($1,$2,'admin',$3)
		RETURNING id,display_name,COALESCE(namespace,''),role,disabled,created_at`, id, displayName, namespace).
		Scan(&user.ID, &user.DisplayName, &user.Namespace, &user.Role, &user.Disabled, &user.Created); err != nil {
		return User{}, IssuedToken{}, fmt.Errorf("create migrated administrator: %w", err)
	}
	issued, tokenHash, err := newToken()
	if err != nil {
		return User{}, IssuedToken{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO auth_tokens(id,user_id,token_hash,expires_at) VALUES($1,$2,$3,$4)`, issued.ID, user.ID, tokenHash, issued.ExpiresAt); err != nil {
		return User{}, IssuedToken{}, fmt.Errorf("issue migrated administrator token: %w", err)
	}
	fingerprint := fmt.Sprintf("sha256:%x", hashToken(legacyToken))
	if _, err := tx.Exec(ctx, `INSERT INTO audit_events(actor_user_id,action,target,succeeded) VALUES($1,'migrate_legacy_token',$2,true)`, user.ID, fingerprint); err != nil {
		return User{}, IssuedToken{}, fmt.Errorf("audit legacy identity migration: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, IssuedToken{}, fmt.Errorf("commit legacy identity migration: %w", err)
	}
	return user, issued, nil
}

func (s *Store) RecoverAdminToken(ctx context.Context, userID string) (IssuedToken, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return IssuedToken{}, fmt.Errorf("begin administrator token recovery: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var role Role
	var disabled bool
	if err := tx.QueryRow(ctx, `SELECT role,disabled FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&role, &disabled); err != nil {
		return IssuedToken{}, fmt.Errorf("find administrator for recovery: %w", err)
	}
	if role != RoleAdmin || disabled {
		return IssuedToken{}, fmt.Errorf("active administrator is required")
	}
	if _, err := tx.Exec(ctx, `UPDATE auth_tokens SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL`, userID); err != nil {
		return IssuedToken{}, fmt.Errorf("revoke previous administrator tokens: %w", err)
	}
	issued, hash, err := newToken()
	if err != nil {
		return IssuedToken{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO auth_tokens(id,user_id,token_hash,expires_at) VALUES($1,$2,$3,$4)`, issued.ID, userID, hash, issued.ExpiresAt); err != nil {
		return IssuedToken{}, fmt.Errorf("issue recovered administrator token: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit_events(actor_user_id,action,target,succeeded) VALUES($1,'recover_admin_token',$1,true)`, userID); err != nil {
		return IssuedToken{}, fmt.Errorf("audit administrator token recovery: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return IssuedToken{}, fmt.Errorf("commit administrator token recovery: %w", err)
	}
	return issued, nil
}

func (s *Store) InitializeBootstrapToken(ctx context.Context, token string) error {
	if token == "" {
		return fmt.Errorf("bootstrap token is required")
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO bootstrap_state(id,token_hash) VALUES(true,$1) ON CONFLICT(id) DO NOTHING`, hashToken(token))
	if err != nil {
		return fmt.Errorf("initialize administrator bootstrap: %w", err)
	}
	return nil
}

func newToken() (IssuedToken, []byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return IssuedToken{}, nil, fmt.Errorf("generate Buddy token: %w", err)
	}
	token := "bbt_" + base64.RawURLEncoding.EncodeToString(raw)
	idRaw := make([]byte, 16)
	if _, err := rand.Read(idRaw); err != nil {
		return IssuedToken{}, nil, fmt.Errorf("generate token ID: %w", err)
	}
	return IssuedToken{ID: base64.RawURLEncoding.EncodeToString(idRaw), Value: token, ExpiresAt: time.Now().Add(LoginTokenLifetime)}, hashToken(token), nil
}

func randomID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("generate user ID: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func hashToken(token string) []byte {
	hash := sha256.Sum256([]byte(token))
	return hash[:]
}
