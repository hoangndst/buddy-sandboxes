package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
	"github.com/hoangndst/buddy-sandboxes/internal/credentials"
	"github.com/hoangndst/buddy-sandboxes/internal/identity"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type credentialTestKey struct {
	userID  string
	service string
	scope   string
	sandbox string
}

type memoryCredentialStore struct {
	values  map[credentialTestKey]bool
	deleted []credentialTestKey
}

func (s *memoryCredentialStore) List(context.Context, string) ([]credentials.Metadata, error) {
	return nil, nil
}

func (s *memoryCredentialStore) Set(_ context.Context, userID, service, scope, sandbox, _ string, _ ...credentials.Options) error {
	s.values[credentialTestKey{userID: userID, service: service, scope: scope, sandbox: sandbox}] = true
	return nil
}

func (s *memoryCredentialStore) SetAudited(ctx context.Context, userID, service, scope, sandbox, value string, opts ...credentials.Options) error {
	return s.Set(ctx, userID, service, scope, sandbox, value, opts...)
}

func (s *memoryCredentialStore) Delete(_ context.Context, userID, service, scope, sandbox string) error {
	key := credentialTestKey{userID: userID, service: service, scope: scope, sandbox: sandbox}
	s.deleted = append(s.deleted, key)
	delete(s.values, key)
	return nil
}

func (s *memoryCredentialStore) DeleteAudited(ctx context.Context, userID, service, scope, sandbox string) error {
	return s.Delete(ctx, userID, service, scope, sandbox)
}

type credentialAuditStub struct{}

func (credentialAuditStub) Audit(context.Context, string, string, string, bool) error { return nil }

type memoryGitCredentialStore struct {
	items   []credentials.GitMetadata
	set     []credentialTestKey
	value   string
	deleted []credentialTestKey
}

func (s *memoryGitCredentialStore) List(context.Context, string) ([]credentials.GitMetadata, error) {
	return s.items, nil
}
func (s *memoryGitCredentialStore) Set(_ context.Context, userID, repository, scope, sandbox, value string) error {
	s.set = append(s.set, credentialTestKey{userID: userID, service: repository, scope: scope, sandbox: sandbox})
	s.value = value
	return nil
}
func (s *memoryGitCredentialStore) SetAudited(ctx context.Context, userID, repository, scope, sandbox, value string) error {
	return s.Set(ctx, userID, repository, scope, sandbox, value)
}
func (s *memoryGitCredentialStore) Delete(_ context.Context, userID, repository, scope, sandbox string) error {
	s.deleted = append(s.deleted, credentialTestKey{userID: userID, service: repository, scope: scope, sandbox: sandbox})
	return nil
}
func (s *memoryGitCredentialStore) DeleteAudited(ctx context.Context, userID, repository, scope, sandbox string) error {
	return s.Delete(ctx, userID, repository, scope, sandbox)
}

func TestCredentialDeleteRemovesStaleSandboxScopeForOwnerOnly(t *testing.T) {
	const sandboxName = "removed-sandbox"
	owner := credentialTestKey{userID: "owner-a", service: credentials.ServiceAnthropic, scope: credentials.ScopeSandbox, sandbox: sandboxName}
	otherOwner := credentialTestKey{userID: "owner-b", service: credentials.ServiceAnthropic, scope: credentials.ScopeSandbox, sandbox: sandboxName}
	store := &memoryCredentialStore{values: map[credentialTestKey]bool{owner: true, otherOwner: true}}
	handler := &CredentialHandlers{store: store, users: credentialAuditStub{}}
	request := httptest.NewRequest(http.MethodDelete, credentialsPath+"/anthropic?sandbox="+sandboxName, nil)
	request = request.WithContext(WithPrincipal(request.Context(), identity.Principal{ID: owner.userID, Namespace: "tenant-a", Role: identity.RoleUser}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.Code)
	}
	if store.values[owner] {
		t.Fatal("DELETE did not remove the owner's stale sandbox credential")
	}
	if !store.values[otherOwner] {
		t.Fatal("DELETE removed another owner's credential")
	}
	if len(store.deleted) != 1 || store.deleted[0] != owner {
		t.Fatalf("delete scope = %+v, want owner-scoped key %+v", store.deleted, owner)
	}
}

func TestCredentialPutStillRequiresOwnedSandbox(t *testing.T) {
	store := &memoryCredentialStore{values: make(map[credentialTestKey]bool)}
	scheme := runtime.NewScheme()
	if err := sandboxesv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	handler := &CredentialHandlers{store: store, client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	request := httptest.NewRequest(http.MethodPut, credentialsPath+"/anthropic?sandbox=missing-sandbox", strings.NewReader(`{"value":"provider-key"}`))
	request = request.WithContext(WithPrincipal(request.Context(), identity.Principal{ID: "owner-a", Namespace: "tenant-a", Role: identity.RoleUser}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
	if len(store.values) != 0 {
		t.Fatalf("credential was stored for a missing Sandbox: %+v", store.values)
	}
}

func TestGitCredentialSetUsesBodyAndReturnsNoCredentialValue(t *testing.T) {
	store := &memoryGitCredentialStore{}
	handler := &CredentialHandlers{store: &memoryCredentialStore{values: make(map[credentialTestKey]bool)}, users: credentialAuditStub{}, git: store}
	request := httptest.NewRequest(http.MethodPut, "/api/v1/git-credentials?repository=https%3A%2F%2Fgithub.com%2Facme%2Frepo.git&sandbox=work-a", strings.NewReader(`{"value":"github_pat_example123"}`))
	request = request.WithContext(WithPrincipal(request.Context(), identity.Principal{ID: "user-a", Namespace: "tenant-a", Role: identity.RoleUser}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
		t.Fatalf("unexpected credential response: status=%d body=%q", response.Code, response.Body.String())
	}
	if store.value != "github_pat_example123" || len(store.set) != 1 || store.set[0] != (credentialTestKey{userID: "user-a", service: "https://github.com/acme/repo.git", scope: credentials.ScopeSandbox, sandbox: "work-a"}) {
		t.Fatalf("credential scope was not stored for the active user: values=%+v token=%q", store.set, store.value)
	}
}

func TestGitCredentialRejectsDuplicateRepositoryQuery(t *testing.T) {
	store := &memoryGitCredentialStore{}
	handler := &CredentialHandlers{store: &memoryCredentialStore{values: make(map[credentialTestKey]bool)}, users: credentialAuditStub{}, git: store}
	request := httptest.NewRequest(http.MethodPut, "/api/v1/git-credentials?repository=https%3A%2F%2Fgithub.com%2Facme%2Frepo.git&repository=https%3A%2F%2Fgithub.com%2Facme%2Fother.git", strings.NewReader(`{"value":"github_pat_example123"}`))
	request = request.WithContext(WithPrincipal(request.Context(), identity.Principal{ID: "user-a", Namespace: "tenant-a", Role: identity.RoleUser}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || len(store.set) != 0 {
		t.Fatalf("duplicate repository parameters were not rejected: status=%d sets=%d", response.Code, len(store.set))
	}
}
