package gitbroker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
	"github.com/hoangndst/buddy-sandboxes/internal/credentials"
	"github.com/hoangndst/buddy-sandboxes/internal/identity"
)

type testGitStore struct {
	binding    credentials.GitBinding
	bindingErr error
	value      string
	claimCalls int
}

func (s *testGitStore) Ready(context.Context) error { return nil }
func (s *testGitStore) Binding(context.Context, string, string, string, string, string) (credentials.GitBinding, error) {
	return s.binding, s.bindingErr
}
func (s *testGitStore) ClaimPod(context.Context, string) (string, string, error) { return "", "", nil }
func (s *testGitStore) ClaimGitCredential(context.Context, credentials.GitBinding, string, string, time.Time, string) (string, error) {
	s.claimCalls++
	return s.value, nil
}
func (s *testGitStore) CompleteGitCredential(context.Context, credentials.GitBinding, string, string) error {
	return nil
}

type testUserStore struct {
	user identity.User
	err  error
}

func (s testUserStore) GetUser(context.Context, string) (identity.User, error) { return s.user, s.err }

func newBrokerTestHandler(t *testing.T, store *testGitStore) *Handler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := sandboxesv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	controller := true
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "git-pod", Namespace: "tenant-a", UID: types.UID("pod-uid"), Labels: map[string]string{redemptionLabel: "true"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: sandboxesv1alpha1.GroupVersion.String(), Kind: "Sandbox", Name: "repo", UID: types.UID("sandbox-uid"), Controller: &controller}}}, Spec: corev1.PodSpec{ServiceAccountName: "buddy-sandbox"}, Status: corev1.PodStatus{StartTime: ptrMetaTime(time.Now())}}
	sandbox := &sandboxesv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "repo", Namespace: "tenant-a", UID: types.UID("sandbox-uid"), Annotations: map[string]string{creationIDAnnotation: "creation-1"}}, Spec: sandboxesv1alpha1.SandboxSpec{Workspace: sandboxesv1alpha1.WorkspaceSpec{Git: &sandboxesv1alpha1.GitRepositorySpec{URL: "https://github.com/acme/repo.git"}}}}
	kube := k8sfake.NewSimpleClientset(pod)
	kube.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{Authenticated: true, Audiences: []string{"buddy-git-credential-broker"}, User: authenticationv1.UserInfo{Username: "system:serviceaccount:tenant-a:buddy-sandbox", Groups: []string{"system:serviceaccounts:tenant-a"}, Extra: map[string]authenticationv1.ExtraValue{"authentication.kubernetes.io/pod-name": {"git-pod"}, "authentication.kubernetes.io/pod-uid": {"pod-uid"}}}}}, nil
	})
	objects := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()
	store.binding = credentials.GitBinding{ID: "binding-1", CredentialID: "credential-1", UserID: "user-1", Namespace: "tenant-a", SandboxName: "repo", SandboxUID: "sandbox-uid", CreationID: "creation-1", State: "bound"}
	return NewHandler(kube, objects, store, testUserStore{user: identity.User{Principal: identity.Principal{ID: "user-1"}}}, "buddy-git-credential-broker")
}

func ptrMetaTime(value time.Time) *metav1.Time { result := metav1.NewTime(value); return &result }

func TestRedeemReturnsCredentialOnlyForValidatedPod(t *testing.T) {
	store := &testGitStore{value: "github-token-secret"}
	handler := newBrokerTestHandler(t, store)
	request := httptest.NewRequest(http.MethodPost, RedeemPath, strings.NewReader(`{"repository":"https://github.com/acme/repo.git"}`))
	request.Header.Set("Authorization", "Bearer pod-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "github-token-secret" || store.claimCalls != 1 {
		t.Fatalf("unexpected redemption: code=%d body=%q claims=%d", response.Code, response.Body.String(), store.claimCalls)
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" || response.Header().Get("X-Request-ID") == "" {
		t.Fatalf("redemption response missing cache or correlation headers: %#v", response.Header())
	}
}

func TestPendingBindingIsRetryableAndDoesNotClaim(t *testing.T) {
	store := &testGitStore{bindingErr: credentials.ErrGitBindingPending}
	handler := newBrokerTestHandler(t, store)
	request := httptest.NewRequest(http.MethodPost, RedeemPath, strings.NewReader(`{"repository":"https://github.com/acme/repo.git"}`))
	request.Header.Set("Authorization", "Bearer pod-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" || store.claimCalls != 0 {
		t.Fatalf("pending binding must be retryable without claiming: code=%d retry=%q claims=%d", response.Code, response.Header().Get("Retry-After"), store.claimCalls)
	}
}

func TestAbsentBindingAllowsAnonymousCloneAfterPodValidation(t *testing.T) {
	store := &testGitStore{bindingErr: credentials.ErrGitBindingNotFound}
	handler := newBrokerTestHandler(t, store)
	request := httptest.NewRequest(http.MethodPost, RedeemPath, strings.NewReader(`{"repository":"https://github.com/acme/repo.git"}`))
	request.Header.Set("Authorization", "Bearer pod-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || store.claimCalls != 0 {
		t.Fatalf("absent binding should not yield a credential: code=%d claims=%d", response.Code, store.claimCalls)
	}
}

func TestInvalidMethodAndTokenCannotReceiveCredential(t *testing.T) {
	store := &testGitStore{value: "github-token-secret"}
	handler := newBrokerTestHandler(t, store)
	request := httptest.NewRequest(http.MethodGet, RedeemPath, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unexpected method response: %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, RedeemPath, strings.NewReader(`{"repository":"https://github.com/acme/repo.git"}`))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || strings.Contains(response.Body.String(), "github-token-secret") || store.claimCalls != 0 {
		t.Fatalf("unauthenticated request was not rejected safely: code=%d body=%q claims=%d", response.Code, response.Body.String(), store.claimCalls)
	}
}

func TestDisabledUserDoesNotClaimCredential(t *testing.T) {
	store := &testGitStore{value: "github-token-secret"}
	handler := newBrokerTestHandler(t, store)
	handler.users = testUserStore{user: identity.User{Principal: identity.Principal{ID: "user-1"}, Disabled: true}}
	request := httptest.NewRequest(http.MethodPost, RedeemPath, strings.NewReader(`{"repository":"https://github.com/acme/repo.git"}`))
	request.Header.Set("Authorization", "Bearer pod-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || store.claimCalls != 0 {
		t.Fatalf("disabled user must not claim a credential: code=%d claims=%d", response.Code, store.claimCalls)
	}
}

func TestIdentityStoreFailureFailsClosed(t *testing.T) {
	store := &testGitStore{value: "github-token-secret"}
	handler := newBrokerTestHandler(t, store)
	handler.users = testUserStore{err: errors.New("database unavailable")}
	request := httptest.NewRequest(http.MethodPost, RedeemPath, strings.NewReader(`{"repository":"https://github.com/acme/repo.git"}`))
	request.Header.Set("Authorization", "Bearer pod-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || store.claimCalls != 0 {
		t.Fatalf("identity failure must fail closed: code=%d claims=%d", response.Code, store.claimCalls)
	}
}
