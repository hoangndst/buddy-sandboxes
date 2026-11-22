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

func (s *memoryCredentialStore) Set(_ context.Context, userID, service, scope, sandbox, _ string) error {
	s.values[credentialTestKey{userID: userID, service: service, scope: scope, sandbox: sandbox}] = true
	return nil
}

func (s *memoryCredentialStore) Delete(_ context.Context, userID, service, scope, sandbox string) error {
	key := credentialTestKey{userID: userID, service: service, scope: scope, sandbox: sandbox}
	s.deleted = append(s.deleted, key)
	delete(s.values, key)
	return nil
}

type credentialAuditStub struct{}

func (credentialAuditStub) Audit(context.Context, string, string, string, bool) error { return nil }

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
