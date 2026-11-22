package credentials

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Capability struct {
	UserID      string
	Namespace   string
	SandboxName string
	SandboxUID  string
	CreationID  string
	Service     string
	Routes      []string
	ExpiresAt   time.Time
}

type Capabilities struct {
	pool *pgxpool.Pool
}

func NewCapabilities(pool *pgxpool.Pool) *Capabilities {
	return &Capabilities{pool: pool}
}

func (s *Capabilities) IssuePending(ctx context.Context, userID, namespace, sandboxName, creationID, service string, expiresAt time.Time, routes []string) (string, []byte, error) {
	if !validService(service) || namespace == "" || sandboxName == "" || creationID == "" || len(routes) == 0 || !expiresAt.After(time.Now()) {
		return "", nil, fmt.Errorf("invalid broker capability")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("generate broker capability")
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(token))
	_, err := s.pool.Exec(ctx, `INSERT INTO broker_capabilities(token_hash,user_id,namespace,sandbox_name,creation_id,service,allowed_routes,state,expires_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,'pending',$8)`, hash[:], userID, namespace, sandboxName, creationID, service, routes, expiresAt)
	if err != nil {
		return "", nil, fmt.Errorf("persist broker capability")
	}
	return token, hash[:], nil
}

func (s *Capabilities) Activate(ctx context.Context, tokenHash []byte, sandboxUID string) error {
	if sandboxUID == "" {
		return fmt.Errorf("Sandbox UID is required")
	}
	result, err := s.pool.Exec(ctx, `UPDATE broker_capabilities SET sandbox_uid=$2,state='active'
		WHERE token_hash=$1 AND state='pending' AND expires_at>now()`, tokenHash, sandboxUID)
	if err != nil {
		return fmt.Errorf("activate broker capability")
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("pending broker capability is unavailable")
	}
	return nil
}

func (s *Capabilities) Revoke(ctx context.Context, tokenHash []byte) error {
	_, err := s.pool.Exec(ctx, `UPDATE broker_capabilities SET state='revoked',revoked_at=now() WHERE token_hash=$1 AND state<>'revoked'`, tokenHash)
	if err != nil {
		return fmt.Errorf("revoke broker capability")
	}
	return nil
}

func (s *Capabilities) RevokeSandbox(ctx context.Context, namespace, sandboxName, creationID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE broker_capabilities SET state='revoked',revoked_at=now()
		WHERE namespace=$1 AND sandbox_name=$2 AND creation_id=$3 AND state<>'revoked'`, namespace, sandboxName, creationID)
	if err != nil {
		return fmt.Errorf("revoke Sandbox broker capabilities")
	}
	return nil
}

func (s *Capabilities) Authenticate(ctx context.Context, token, method, path string) (Capability, error) {
	if token == "" {
		return Capability{}, fmt.Errorf("unauthorized")
	}
	hash := sha256.Sum256([]byte(token))
	var capability Capability
	var routes []string
	err := s.pool.QueryRow(ctx, `SELECT c.user_id,c.namespace,c.sandbox_name,c.sandbox_uid,c.creation_id,c.service,c.allowed_routes,c.expires_at
		FROM broker_capabilities c JOIN users u ON u.id=c.user_id
		WHERE c.token_hash=$1 AND c.state='active' AND c.revoked_at IS NULL AND c.expires_at>now() AND u.disabled=false`, hash[:]).
		Scan(&capability.UserID, &capability.Namespace, &capability.SandboxName, &capability.SandboxUID, &capability.CreationID,
			&capability.Service, &routes, &capability.ExpiresAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return Capability{}, fmt.Errorf("unauthorized")
		}
		return Capability{}, fmt.Errorf("authenticate broker capability")
	}
	if !containsRoute(routes, method, path) {
		return Capability{}, fmt.Errorf("unauthorized")
	}
	capability.Routes = routes
	return capability, nil
}

func containsRoute(routes []string, method, path string) bool {
	for _, route := range routes {
		if route == method+" "+path {
			return true
		}
	}
	return false
}
