package credentials

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/hoangndst/buddy-sandboxes/internal/gitrepo"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrGitBindingNotFound = fmt.Errorf("Git credential binding not found")
var ErrGitBindingPending = fmt.Errorf("Git credential binding pending")
var ErrGitBindingRevoked = fmt.Errorf("Git credential binding revoked")

const (
	gitHubHost        = "github.com"
	gitClaimSucceeded = "succeeded"
)

// GitMetadata describes a repository credential without revealing its value.
type GitMetadata struct {
	Service     string    `json:"service"`
	Scope       string    `json:"scope"`
	SandboxName string    `json:"sandboxName,omitempty"`
	Repository  string    `json:"repository"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// GitBinding identifies the credential selected for one API-created Sandbox.
type GitBinding struct {
	ID             string
	CredentialID   string
	UserID         string
	Namespace      string
	SandboxName    string
	SandboxUID     string
	CreationID     string
	Host           string
	RepositoryPath string
	State          string
}

type GitBindingRecord struct {
	ID, Namespace, SandboxName, SandboxUID, CreationID, State string
}

// GitSandboxCredentialService is the platform-core boundary used by sandbox creation.
type GitSandboxCredentialService interface {
	PrepareBinding(context.Context, string, string, string, string, GitHubRepository) (GitBinding, bool, error)
	Bind(context.Context, string, string) error
	RollbackBinding(context.Context, string) error
	RevokeSandbox(context.Context, string, string, string, string) error
}

// GitHubRepository is a validated repository identity used for managed clone credentials.
type GitHubRepository struct {
	url  string
	host string
	path string
}

func (r GitHubRepository) CloneURL() string       { return r.url }
func (r GitHubRepository) Hostname() string       { return r.host }
func (r GitHubRepository) RepositoryPath() string { return r.path }

// GitStore manages repository-bound GitHub credentials and Sandbox bindings.
type GitStore struct {
	pool *pgxpool.Pool
	keys *KeySet
}

func NewGitStore(pool *pgxpool.Pool, keys *KeySet) *GitStore {
	return &GitStore{pool: pool, keys: keys}
}

func (s *GitStore) Ready(ctx context.Context) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("Git credential store is unavailable")
	}
	return s.pool.Ping(ctx)
}

func ParseGitHubRepository(raw string) (GitHubRepository, error) {
	if err := gitrepo.ValidateCredentialURL(raw); err != nil {
		return GitHubRepository{}, fmt.Errorf("invalid GitHub repository URL")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host != gitHubHost {
		return GitHubRepository{}, fmt.Errorf("repository must use github.com")
	}
	repositoryPath := strings.TrimPrefix(u.Path, "/")
	parts := strings.Split(repositoryPath, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(repositoryPath, "\\\x00") {
		return GitHubRepository{}, fmt.Errorf("repository path must contain exactly owner/repository")
	}
	return GitHubRepository{url: raw, host: gitHubHost, path: repositoryPath}, nil
}

func validGitToken(token string) bool {
	if token == "" || len(token) > MaxCredentialLen || strings.TrimSpace(token) != token {
		return false
	}
	for _, char := range token {
		if unicode.IsControl(char) || unicode.IsSpace(char) {
			return false
		}
	}
	return true
}

func (s *GitStore) Set(ctx context.Context, userID, repository, scope, sandboxName, token string) error {
	return s.set(ctx, userID, repository, scope, sandboxName, token, false)
}

// SetAudited stores a GitHub credential and its successful audit event atomically.
func (s *GitStore) SetAudited(ctx context.Context, userID, repository, scope, sandboxName, token string) error {
	return s.set(ctx, userID, repository, scope, sandboxName, token, true)
}

func (s *GitStore) set(ctx context.Context, userID, repository, scope, sandboxName, token string, audited bool) error {
	repo, err := ParseGitHubRepository(repository)
	if err != nil || userID == "" || !validGitToken(token) ||
		(scope != ScopeUser && scope != ScopeSandbox) ||
		(scope == ScopeUser && sandboxName != "") ||
		(scope == ScopeSandbox && sandboxName == "") {
		return fmt.Errorf("invalid GitHub credential")
	}
	ciphertext, version, err := s.keys.Encrypt([]byte(token))
	if err != nil {
		return fmt.Errorf("encrypt GitHub credential")
	}
	id, err := randomCredentialID()
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin GitHub credential transaction")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	targetKey := repo.host + "/" + repo.path
	if scope == ScopeUser {
		_, err = tx.Exec(ctx, `INSERT INTO service_credentials(id,user_id,service,scope,sandbox_name,target_key,ciphertext,key_version)
			VALUES($1,$2,'github','user',NULL,$3,$4,$5)
			ON CONFLICT(user_id,service,target_key) WHERE scope='user'
			DO UPDATE SET ciphertext=EXCLUDED.ciphertext,key_version=EXCLUDED.key_version,updated_at=now()`,
			id, userID, targetKey, ciphertext, version)
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO service_credentials(id,user_id,service,scope,sandbox_name,target_key,ciphertext,key_version,expires_at)
			VALUES($1,$2,'github','sandbox',$3,$4,$5,$6,now()+interval '24 hours')
			ON CONFLICT(user_id,service,target_key,sandbox_name) WHERE scope='sandbox'
			DO UPDATE SET ciphertext=EXCLUDED.ciphertext,key_version=EXCLUDED.key_version,expires_at=EXCLUDED.expires_at,updated_at=now()`,
			id, userID, sandboxName, targetKey, ciphertext, version)
	}
	if err != nil {
		return fmt.Errorf("store GitHub credential")
	}
	if audited {
		if err := writeCredentialAudit(ctx, tx, userID, "set_service_credential", targetKey+"/"+scope+"/"+sandboxName); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit GitHub credential")
	}
	return nil
}

func (s *GitStore) List(ctx context.Context, userID string) ([]GitMetadata, error) {
	rows, err := s.pool.Query(ctx, `SELECT scope,COALESCE(sandbox_name,''),target_key,updated_at
		FROM service_credentials WHERE user_id=$1 AND service='github' ORDER BY target_key,scope,sandbox_name`, userID)
	if err != nil {
		return nil, fmt.Errorf("list GitHub credentials")
	}
	defer rows.Close()
	items := make([]GitMetadata, 0)
	for rows.Next() {
		var item GitMetadata
		if err := rows.Scan(&item.Scope, &item.SandboxName, &item.Repository, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("read GitHub credential metadata")
		}
		item.Service = ServiceGitHub
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read GitHub credential metadata")
	}
	return items, nil
}

func (s *GitStore) Delete(ctx context.Context, userID, repository, scope, sandboxName string) error {
	return s.delete(ctx, userID, repository, scope, sandboxName, false)
}

// DeleteAudited removes a GitHub credential and records the successful operation atomically.
func (s *GitStore) DeleteAudited(ctx context.Context, userID, repository, scope, sandboxName string) error {
	return s.delete(ctx, userID, repository, scope, sandboxName, true)
}

func (s *GitStore) delete(ctx context.Context, userID, repository, scope, sandboxName string, audited bool) error {
	repo, err := ParseGitHubRepository(repository)
	if err != nil || userID == "" || (scope != ScopeUser && scope != ScopeSandbox) ||
		(scope == ScopeUser && sandboxName != "") || (scope == ScopeSandbox && sandboxName == "") {
		return fmt.Errorf("invalid GitHub credential scope")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin GitHub credential removal")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if scope == ScopeUser {
		_, err = tx.Exec(ctx, `DELETE FROM service_credentials WHERE user_id=$1 AND service='github' AND target_key=$2 AND scope='user'`, userID, repo.host+"/"+repo.path)
	} else {
		_, err = tx.Exec(ctx, `DELETE FROM service_credentials WHERE user_id=$1 AND service='github' AND target_key=$2 AND scope='sandbox' AND sandbox_name=$3`, userID, repo.host+"/"+repo.path, sandboxName)
	}
	if err != nil {
		return fmt.Errorf("remove GitHub credential")
	}
	if audited {
		if err := writeCredentialAudit(ctx, tx, userID, "remove_service_credential", repo.host+"/"+repo.path+"/"+scope+"/"+sandboxName); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit GitHub credential removal")
	}
	return nil
}

// PrepareBinding selects a repository credential and stores a pending API-owned binding.
func (s *GitStore) PrepareBinding(ctx context.Context, userID, namespace, sandboxName, creationID string, repo GitHubRepository) (GitBinding, bool, error) {
	if repo.host != gitHubHost || repo.url == "" || repo.path == "" {
		return GitBinding{}, false, fmt.Errorf("invalid GitHub repository identity")
	}
	var binding GitBinding
	var err error
	binding.ID, err = randomCredentialID()
	if err != nil {
		return GitBinding{}, false, err
	}
	binding.UserID, binding.Namespace, binding.SandboxName = userID, namespace, sandboxName
	binding.CreationID, binding.Host, binding.RepositoryPath = creationID, repo.host, repo.path
	err = s.pool.QueryRow(ctx, `SELECT id FROM service_credentials WHERE user_id=$1 AND service='github' AND target_key=$2
		AND (scope='user' OR expires_at>now())
		AND (scope='user' OR (scope='sandbox' AND sandbox_name=$3))
		ORDER BY CASE WHEN scope='sandbox' THEN 0 ELSE 1 END LIMIT 1`, userID, repo.host+"/"+repo.path, sandboxName).Scan(&binding.CredentialID)
	if err != nil {
		if err == pgx.ErrNoRows {
			return GitBinding{}, false, nil
		}
		return GitBinding{}, false, fmt.Errorf("select GitHub credential")
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO git_sandbox_bindings(id,user_id,credential_id,namespace,sandbox_name,creation_id,host,repository_path,state,expires_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,'pending',now()+interval '5 minutes')`,
		binding.ID, binding.UserID, binding.CredentialID, binding.Namespace, binding.SandboxName, binding.CreationID, repo.host, repo.path)
	if err != nil {
		return GitBinding{}, false, fmt.Errorf("prepare GitHub credential binding")
	}
	binding.State = "pending"
	return binding, true, nil
}

func (s *GitStore) Bind(ctx context.Context, bindingID, sandboxUID string) error {
	if bindingID == "" || sandboxUID == "" {
		return fmt.Errorf("invalid GitHub credential binding")
	}
	tag, err := s.pool.Exec(ctx, `UPDATE git_sandbox_bindings SET sandbox_uid=$2,state='bound'
		WHERE id=$1 AND state='pending' AND expires_at>now()`, bindingID, sandboxUID)
	if err != nil || tag.RowsAffected() != 1 {
		return fmt.Errorf("bind GitHub credential to Sandbox")
	}
	return nil
}

func (s *GitStore) BindingByIdentity(ctx context.Context, namespace, name, sandboxUID, creationID, repository string) (GitBinding, error) {
	return s.Binding(ctx, namespace, name, sandboxUID, creationID, repository)
}

func (s *GitStore) ClaimPod(ctx context.Context, bindingID string) (string, string, error) {
	var name, uid string
	err := s.pool.QueryRow(ctx, `SELECT pod_name,pod_uid FROM git_clone_claims WHERE binding_id=$1`, bindingID).Scan(&name, &uid)
	if err == pgx.ErrNoRows {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read Git credential claim identity")
	}
	return name, uid, nil
}

// ClaimGitCredential validates owner activity and atomically leases delivery to a Pod UID.
func (s *GitStore) ClaimGitCredential(ctx context.Context, binding GitBinding, podName, podUID string, deadline time.Time, checkedPriorPodUID string) (string, error) {
	if binding.ID == "" || podName == "" || podUID == "" || deadline.IsZero() {
		return "", fmt.Errorf("invalid Git credential claim")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin Git credential claim")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var disabled bool
	if err := tx.QueryRow(ctx, `SELECT disabled FROM users WHERE id=$1 FOR SHARE`, binding.UserID).Scan(&disabled); err != nil {
		return "", fmt.Errorf("verify Git credential owner")
	}
	if disabled {
		return "", fmt.Errorf("Git credential owner is disabled")
	}
	var state, boundUID, creationID, credentialID string
	if err := tx.QueryRow(ctx, `SELECT state,sandbox_uid,creation_id,credential_id FROM git_sandbox_bindings WHERE id=$1 FOR UPDATE`, binding.ID).
		Scan(&state, &boundUID, &creationID, &credentialID); err != nil || state != "bound" || boundUID != binding.SandboxUID || creationID != binding.CreationID || credentialID != binding.CredentialID {
		return "", fmt.Errorf("Git credential binding is unavailable")
	}
	var priorPodUID string
	var priorPodName string
	var attempts int
	var expiresAt time.Time
	var outcome string
	err = tx.QueryRow(ctx, `SELECT pod_name,pod_uid,attempts,deadline,outcome FROM git_clone_claims WHERE binding_id=$1 FOR UPDATE`, binding.ID).
		Scan(&priorPodName, &priorPodUID, &attempts, &expiresAt, &outcome)
	if err == pgx.ErrNoRows {
		_, err = tx.Exec(ctx, `INSERT INTO git_clone_claims(binding_id,sandbox_uid,creation_id,pod_name,pod_uid,attempts,deadline,outcome)
			VALUES($1,$2,$3,$4,$5,1,$6,'claimed')`, binding.ID, binding.SandboxUID, binding.CreationID, podName, podUID, deadline)
		if err != nil {
			return "", fmt.Errorf("create Git credential claim")
		}
		attempts = 1
	} else if err != nil {
		return "", fmt.Errorf("read Git credential claim")
	} else {
		if priorPodUID != podUID {
			if !canReplaceGitClaim(priorPodUID, checkedPriorPodUID) {
				return "", fmt.Errorf("Git credential is claimed by another Pod")
			}
			_, err = tx.Exec(ctx, `UPDATE git_clone_claims SET pod_name=$2,pod_uid=$3,attempts=1,deadline=$4,outcome='claimed',updated_at=now() WHERE binding_id=$1`, binding.ID, podName, podUID, deadline)
			if err != nil {
				return "", fmt.Errorf("replace Git credential claim")
			}
			attempts = 1
		} else if outcome == gitClaimSucceeded || time.Now().After(expiresAt) || attempts >= 3 {
			return "", fmt.Errorf("Git credential claim is no longer active")
		} else {
			_, err = tx.Exec(ctx, `UPDATE git_clone_claims SET pod_name=$2,attempts=attempts+1,updated_at=now() WHERE binding_id=$1`, binding.ID, podName)
			if err != nil {
				return "", fmt.Errorf("retry Git credential claim")
			}
		}
	}
	var ciphertext []byte
	var keyVersion int
	err = tx.QueryRow(ctx, `SELECT ciphertext,key_version FROM service_credentials WHERE id=$1 AND user_id=$2 AND service='github'`, credentialID, binding.UserID).Scan(&ciphertext, &keyVersion)
	if err != nil {
		return "", fmt.Errorf("Git credential is unavailable")
	}
	plaintext, err := s.keys.Decrypt(keyVersion, ciphertext)
	if err != nil {
		return "", fmt.Errorf("decrypt Git credential")
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit Git credential claim")
	}
	return string(plaintext), nil
}

func canReplaceGitClaim(currentPodUID, checkedPriorPodUID string) bool {
	return checkedPriorPodUID != "" && currentPodUID == checkedPriorPodUID
}

func (s *GitStore) CompleteGitCredential(ctx context.Context, binding GitBinding, podUID, outcome string) error {
	if outcome != gitClaimSucceeded && outcome != "failed" {
		return fmt.Errorf("invalid Git credential outcome")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin Git credential completion")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var state string
	var currentPod, currentOutcome string
	err = tx.QueryRow(ctx, `SELECT b.state,c.pod_uid,c.outcome FROM git_sandbox_bindings b
		JOIN git_clone_claims c ON c.binding_id=b.id WHERE b.id=$1 AND b.sandbox_uid=$2 AND b.creation_id=$3 FOR UPDATE OF b,c`,
		binding.ID, binding.SandboxUID, binding.CreationID).Scan(&state, &currentPod, &currentOutcome)
	if err != nil || state != "bound" || currentPod != podUID {
		return fmt.Errorf("Git credential claim is unavailable")
	}
	if currentOutcome == outcome {
		return tx.Commit(ctx)
	}
	if currentOutcome == gitClaimSucceeded {
		return fmt.Errorf("Git credential claim already succeeded")
	}
	_, err = tx.Exec(ctx, `UPDATE git_clone_claims SET outcome=$2,updated_at=now() WHERE binding_id=$1`, binding.ID, outcome)
	if err != nil {
		return fmt.Errorf("complete Git credential claim")
	}
	return tx.Commit(ctx)
}

func (s *GitStore) RevokeSandbox(ctx context.Context, namespace, name, uid, creationID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE git_sandbox_bindings SET state='revoked' WHERE namespace=$1 AND sandbox_name=$2 AND sandbox_uid=$3 AND creation_id=$4`, namespace, name, uid, creationID)
	if err != nil {
		return fmt.Errorf("revoke Git credential binding")
	}
	return nil
}

func (s *GitStore) ListBindings(ctx context.Context) ([]GitBindingRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,namespace,sandbox_name,sandbox_uid,creation_id,state FROM git_sandbox_bindings`)
	if err != nil {
		return nil, fmt.Errorf("list Git credential bindings")
	}
	defer rows.Close()
	items := make([]GitBindingRecord, 0)
	for rows.Next() {
		var item GitBindingRecord
		if err := rows.Scan(&item.ID, &item.Namespace, &item.SandboxName, &item.SandboxUID, &item.CreationID, &item.State); err != nil {
			return nil, fmt.Errorf("read Git credential bindings")
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read Git credential bindings")
	}
	return items, nil
}

func (s *GitStore) ExpirePendingBindings(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM git_sandbox_bindings WHERE state='pending' AND expires_at<=now()`)
	if err != nil {
		return fmt.Errorf("expire pending Git credential bindings")
	}
	return nil
}

func (s *GitStore) ExpireUnclaimedSandboxCredentials(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM service_credentials g WHERE service='github' AND scope='sandbox' AND expires_at<=now()
		AND NOT EXISTS (SELECT 1 FROM git_sandbox_bindings b WHERE b.credential_id=g.id)`)
	if err != nil {
		return fmt.Errorf("expire unclaimed sandbox Git credentials")
	}
	return nil
}

// CleanupBinding deletes a sandbox-scoped credential only after its exact binding is gone.
func (s *GitStore) CleanupBinding(ctx context.Context, bindingID, namespace, name, uid, creationID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin Git binding cleanup")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var credentialID, scope string
	err = tx.QueryRow(ctx, `SELECT credential_id,(SELECT scope FROM service_credentials WHERE id=credential_id)
		FROM git_sandbox_bindings WHERE id=$1 AND namespace=$2 AND sandbox_name=$3 AND sandbox_uid=$4 AND creation_id=$5 FOR UPDATE`,
		bindingID, namespace, name, uid, creationID).Scan(&credentialID, &scope)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("verify Git binding cleanup identity")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM git_sandbox_bindings WHERE id=$1 AND namespace=$2 AND sandbox_name=$3 AND sandbox_uid=$4 AND creation_id=$5`, bindingID, namespace, name, uid, creationID); err != nil {
		return fmt.Errorf("delete Git binding")
	}
	if scope == ScopeSandbox {
		if _, err := tx.Exec(ctx, `DELETE FROM service_credentials WHERE id=$1 AND service='github'`, credentialID); err != nil {
			return fmt.Errorf("delete sandbox Git credential")
		}
	}
	return tx.Commit(ctx)
}

func (s *GitStore) DeleteBinding(ctx context.Context, namespace, name, uid, creationID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM git_sandbox_bindings WHERE namespace=$1 AND sandbox_name=$2 AND sandbox_uid=$3 AND creation_id=$4`, namespace, name, uid, creationID)
	if err != nil {
		return fmt.Errorf("delete Git credential binding")
	}
	return nil
}

func (s *GitStore) RollbackBinding(ctx context.Context, bindingID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM git_sandbox_bindings WHERE id=$1 AND state='pending'`, bindingID)
	if err != nil {
		return fmt.Errorf("remove pending GitHub credential binding")
	}
	return nil
}

// Binding returns a bound record only for an exact Sandbox identity and repository.
func (s *GitStore) Binding(ctx context.Context, namespace, name, sandboxUID, creationID, repository string) (GitBinding, error) {
	repo, err := ParseGitHubRepository(repository)
	if err != nil {
		return GitBinding{}, err
	}
	var b GitBinding
	err = s.pool.QueryRow(ctx, `SELECT id,user_id,credential_id,namespace,sandbox_name,sandbox_uid,creation_id,host,repository_path,state
		FROM git_sandbox_bindings WHERE namespace=$1 AND sandbox_name=$2 AND creation_id=$3
		AND host=$4 AND repository_path=$5`, namespace, name, creationID, repo.host, repo.path).
		Scan(&b.ID, &b.UserID, &b.CredentialID, &b.Namespace, &b.SandboxName, &b.SandboxUID, &b.CreationID, &b.Host, &b.RepositoryPath, &b.State)
	if err != nil {
		if err == pgx.ErrNoRows {
			return GitBinding{}, ErrGitBindingNotFound
		}
		return GitBinding{}, fmt.Errorf("GitHub credential binding unavailable")
	}
	if b.State == "pending" {
		return GitBinding{}, ErrGitBindingPending
	}
	if b.State == "revoked" {
		return GitBinding{}, ErrGitBindingRevoked
	}
	if b.SandboxUID != sandboxUID {
		return GitBinding{}, ErrGitBindingNotFound
	}
	return b, nil
}
