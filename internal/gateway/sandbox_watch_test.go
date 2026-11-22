package gateway

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
)

func TestSandboxStateCacheRequiresFreshRunningMatchingSandbox(t *testing.T) {
	now := time.Now()
	cache := &SandboxStateCache{namespace: "default", items: map[string]sandboxState{}, now: func() time.Time { return now }}
	sandbox := &sandboxesv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{
		Name: "work", Namespace: "default", Annotations: map[string]string{sandboxCreationIDAnnotation: encodedToken(1)},
	}, Status: sandboxesv1alpha1.SandboxStatus{Phase: sandboxesv1alpha1.SandboxPhaseRunning}}
	cache.replace(&sandboxesv1alpha1.SandboxList{Items: []sandboxesv1alpha1.Sandbox{*sandbox}})
	if !cache.Ready() || !cache.Allows("default", "work", encodedToken(1)) {
		t.Fatal("fresh matching Running Sandbox denied")
	}
	if cache.Allows("other", "work", encodedToken(1)) || cache.Allows("default", "work", encodedToken(2)) {
		t.Fatal("namespace or creation ID mismatch allowed")
	}
	cache.markUnhealthy()
	if cache.Ready() || cache.Allows("default", "work", encodedToken(1)) {
		t.Fatal("watch error did not fail closed")
	}
	cache.replace(&sandboxesv1alpha1.SandboxList{Items: []sandboxesv1alpha1.Sandbox{*sandbox}})
	if !cache.Ready() || !cache.Allows("default", "work", encodedToken(1)) {
		t.Fatal("successful authoritative refresh did not recover cache")
	}
	now = now.Add(sandboxCacheMaxAge + time.Second)
	if cache.Ready() || cache.Allows("default", "work", encodedToken(1)) {
		t.Fatal("stale cache did not fail closed")
	}
}

func TestSandboxStateCacheWatchRevokesSession(t *testing.T) {
	scheme := setupTestScheme(t)
	creationID := encodedToken(3)
	sandbox := &sandboxesv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{
		Name: "watched-work", Namespace: "default", Annotations: map[string]string{sandboxCreationIDAnnotation: creationID},
	}, Status: sandboxesv1alpha1.SandboxStatus{Phase: sandboxesv1alpha1.SandboxPhaseRunning}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&sandboxesv1alpha1.Sandbox{}).WithObjects(sandbox).Build()
	cache := NewSandboxStateCache(kubeClient, "default")
	store := NewCredentialSessionStore()
	session := &credentialSession{key: credentialSessionKey{namespace: "default", sandbox: sandbox.Name, creationID: creationID}, expiresAt: time.Now().Add(time.Hour)}
	store.byKey[session.key] = session
	cache.SetInvalidationHandler(store.Revoke)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cache.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for !cache.Allows("default", sandbox.Name, creationID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !cache.Allows("default", sandbox.Name, creationID) {
		t.Fatal("informer did not authorize synced Sandbox")
	}
	updated := &sandboxesv1alpha1.Sandbox{}
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(sandbox), updated); err != nil {
		t.Fatal(err)
	}
	updated.Spec.Suspend = true
	if err := kubeClient.Update(ctx, updated); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for store.isActive(session) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if store.isActive(session) {
		t.Fatal("Sandbox suspension did not revoke the provider session")
	}
}

func TestSandboxStateCacheRevokesOnDeleteSuspendAndTerminalState(t *testing.T) {
	cache := &SandboxStateCache{namespace: "default", items: map[string]sandboxState{}, now: time.Now}
	creationID := encodedToken(1)
	sandbox := &sandboxesv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{
		Name: "work", Namespace: "default", Annotations: map[string]string{sandboxCreationIDAnnotation: creationID},
	}, Status: sandboxesv1alpha1.SandboxStatus{Phase: sandboxesv1alpha1.SandboxPhaseRunning}}
	cache.replace(&sandboxesv1alpha1.SandboxList{Items: []sandboxesv1alpha1.Sandbox{*sandbox}})
	sandbox.Spec.Suspend = true
	cache.applyObject(sandbox)
	if cache.Allows("default", "work", creationID) {
		t.Fatal("suspended Sandbox allowed")
	}
	sandbox.Spec.Suspend = false
	sandbox.Status.Phase = sandboxesv1alpha1.SandboxPhaseFailed
	cache.applyObject(sandbox)
	if cache.Allows("default", "work", creationID) {
		t.Fatal("terminal Sandbox allowed")
	}
	sandbox.Status.Phase = sandboxesv1alpha1.SandboxPhaseRunning
	cache.applyObject(sandbox)
	cache.deleteObject(sandbox)
	if cache.Allows("default", "work", creationID) {
		t.Fatal("deleted Sandbox allowed")
	}
}
