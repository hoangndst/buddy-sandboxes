package gateway

import (
	"context"
	"log"
	"sync"
	"time"

	corev1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
	"github.com/hoangndst/buddy-sandboxes/internal/identity"
)

const (
	sandboxRefreshInterval = 15 * time.Second
	sandboxCacheMaxAge     = 30 * time.Second
)

type sandboxState struct {
	creationID string
	uid        string
	phase      sandboxesv1alpha1.SandboxPhase
	suspended  bool
	deleting   bool
}

// SandboxStateCache tracks one namespace through Kubernetes list/watch and periodic refreshes.
type SandboxStateCache struct {
	client       client.WithWatch
	namespace    string
	mu           sync.RWMutex
	items        map[string]sandboxState
	lastSync     time.Time
	ready        bool
	watchErr     bool
	now          func() time.Time
	informer     cache.SharedIndexInformer
	onInvalidate func(namespace, sandboxName, creationID string)
}

// NewSandboxStateCache creates a namespace-scoped state cache for provider authorization.
func NewSandboxStateCache(kubeClient client.WithWatch, namespace string) *SandboxStateCache {
	state := &SandboxStateCache{client: kubeClient, namespace: namespace, items: make(map[string]sandboxState), now: time.Now}
	listWatch := &cache.ListWatch{
		ListFunc: func(opts corev1.ListOptions) (runtime.Object, error) {
			list := &sandboxesv1alpha1.SandboxList{}
			err := kubeClient.List(context.Background(), list, &client.ListOptions{Namespace: namespace, Raw: &opts})
			return list, err
		},
		WatchFunc: func(opts corev1.ListOptions) (watch.Interface, error) {
			watcher, err := kubeClient.Watch(context.Background(), &sandboxesv1alpha1.SandboxList{}, &client.ListOptions{Namespace: namespace, Raw: &opts})
			if err != nil {
				log.Printf("watch Sandbox state in namespace %q: %v", namespace, err)
			}
			return watcher, err
		},
	}
	state.informer = cache.NewSharedIndexInformer(listWatch, &sandboxesv1alpha1.Sandbox{}, 0, cache.Indexers{})
	_, _ = state.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    state.applyObject,
		UpdateFunc: func(_, current any) { state.applyObject(current) },
		DeleteFunc: state.deleteObject,
	})
	if err := state.informer.SetWatchErrorHandler(func(_ *cache.Reflector, watchErr error) {
		log.Printf("Sandbox state watch failed: %v", watchErr)
		state.markUnhealthy()
	}); err != nil {
		state.markUnhealthy()
	}
	return state
}

// Run refreshes authoritative state every 15 seconds and watches intervening changes.
func (c *SandboxStateCache) Run(ctx context.Context) {
	go c.informer.Run(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), c.informer.HasSynced) {
		return
	}
	c.refresh(ctx)
	ticker := time.NewTicker(sandboxRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.refresh(ctx)
		}
	}
}

func (c *SandboxStateCache) refresh(ctx context.Context) {
	list := &sandboxesv1alpha1.SandboxList{}
	if err := c.client.List(ctx, list, client.InNamespace(c.namespace)); err != nil {
		log.Printf("refresh Sandbox state in namespace %q: %v", c.namespace, err)
		c.markUnhealthy()
		return
	}
	c.replace(list)
}

func (c *SandboxStateCache) replace(list *sandboxesv1alpha1.SandboxList) {
	items := make(map[string]sandboxState, len(list.Items))
	for i := range list.Items {
		items[list.Items[i].Name] = stateFor(&list.Items[i])
	}
	c.mu.Lock()
	type revoked struct{ name, creationID string }
	var invalidated []revoked
	for name, old := range c.items {
		current, exists := items[name]
		if old.creationID != "" && (!exists || current.creationID != old.creationID || stateRevokesSession(current)) {
			invalidated = append(invalidated, revoked{name: name, creationID: old.creationID})
		}
	}
	for name, current := range items {
		if current.creationID != "" && stateRevokesSession(current) {
			invalidated = append(invalidated, revoked{name: name, creationID: current.creationID})
		}
	}
	c.items = items
	c.lastSync = c.now()
	c.ready = true
	c.watchErr = false
	callback := c.onInvalidate
	c.mu.Unlock()
	if callback != nil {
		for _, item := range invalidated {
			callback(c.namespace, item.name, item.creationID)
		}
	}
}

func (c *SandboxStateCache) applyObject(obj any) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	sandbox, ok := obj.(*sandboxesv1alpha1.Sandbox)
	if !ok {
		c.markUnhealthy()
		return
	}
	c.mu.Lock()
	old, hadOld := c.items[sandbox.Name]
	c.items[sandbox.Name] = stateFor(sandbox)
	callback := c.onInvalidate
	c.mu.Unlock()
	current := stateFor(sandbox)
	if callback != nil && current.creationID != "" && stateRevokesSession(current) {
		callback(c.namespace, sandbox.Name, current.creationID)
	}
	if hadOld && old.creationID != "" && old.creationID != current.creationID && callback != nil {
		callback(c.namespace, sandbox.Name, old.creationID)
	}
}

func (c *SandboxStateCache) deleteObject(obj any) {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	sandbox, ok := obj.(*sandboxesv1alpha1.Sandbox)
	if !ok {
		c.markUnhealthy()
		return
	}
	c.mu.Lock()
	old := c.items[sandbox.Name]
	delete(c.items, sandbox.Name)
	callback := c.onInvalidate
	c.mu.Unlock()
	if old.creationID != "" && callback != nil {
		callback(c.namespace, sandbox.Name, old.creationID)
	}
}

func stateFor(sandbox *sandboxesv1alpha1.Sandbox) sandboxState {
	return sandboxState{
		creationID: sandbox.Annotations[sandboxCreationIDAnnotation],
		uid:        string(sandbox.UID),
		phase:      sandbox.Status.Phase,
		suspended:  sandbox.Spec.Suspend,
		deleting:   sandbox.DeletionTimestamp != nil,
	}
}

func stateCanUseProvider(state sandboxState) bool {
	return state.phase == sandboxesv1alpha1.SandboxPhaseRunning && !state.suspended && !state.deleting
}

func stateRevokesSession(state sandboxState) bool {
	return state.suspended || state.deleting || state.phase == sandboxesv1alpha1.SandboxPhaseSucceeded || state.phase == sandboxesv1alpha1.SandboxPhaseFailed
}

// SetInvalidationHandler installs a callback for Sandbox deletion, suspension, terminal state, or replacement.
func (c *SandboxStateCache) SetInvalidationHandler(handler func(namespace, sandboxName, creationID string)) {
	c.mu.Lock()
	c.onInvalidate = handler
	c.mu.Unlock()
}

func (c *SandboxStateCache) markUnhealthy() {
	c.mu.Lock()
	c.watchErr = true
	c.mu.Unlock()
}

// Allows returns true only for a fresh cache entry matching a live broker-enabled Sandbox.
func (c *SandboxStateCache) Allows(namespace, name, creationID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if namespace != c.namespace || !c.ready || c.watchErr || c.now().Sub(c.lastSync) > sandboxCacheMaxAge {
		return false
	}
	state, ok := c.items[name]
	return ok && state.creationID != "" && state.creationID == creationID && stateCanUseProvider(state)
}

func (c *SandboxStateCache) AllowsSandbox(namespace, name, uid, creationID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if namespace != c.namespace || !c.ready || c.watchErr || c.now().Sub(c.lastSync) > sandboxCacheMaxAge {
		return false
	}
	state, ok := c.items[name]
	return ok && state.uid != "" && state.uid == uid && state.creationID != "" && state.creationID == creationID && stateCanUseProvider(state)
}

// Ready reports whether the last authoritative refresh is fresh and the watch has no known error.
func (c *SandboxStateCache) Ready() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ready && !c.watchErr && c.now().Sub(c.lastSync) <= sandboxCacheMaxAge
}

// MultiSandboxStateCache keeps one scoped informer for each operator-enabled namespace.
type MultiSandboxStateCache struct {
	client       client.WithWatch
	registry     *identity.Store
	mu           sync.RWMutex
	caches       map[string]*SandboxStateCache
	cancels      map[string]context.CancelFunc
	onInvalidate func(namespace, sandboxName, creationID string)
}

func NewMultiSandboxStateCache(kubeClient client.WithWatch, registry *identity.Store) *MultiSandboxStateCache {
	return &MultiSandboxStateCache{
		client: kubeClient, registry: registry,
		caches: make(map[string]*SandboxStateCache), cancels: make(map[string]context.CancelFunc),
	}
}

func (m *MultiSandboxStateCache) Run(ctx context.Context) {
	refresh := func() {
		namespaces, err := m.registry.ListEnabledNamespaces(ctx)
		if err != nil {
			log.Printf("list broker-enabled namespaces: %v", err)
			return
		}
		wanted := make(map[string]struct{}, len(namespaces))
		for _, namespace := range namespaces {
			wanted[namespace] = struct{}{}
		}
		m.mu.Lock()
		for namespace, cancel := range m.cancels {
			if _, ok := wanted[namespace]; !ok {
				cancel()
				delete(m.cancels, namespace)
				delete(m.caches, namespace)
			}
		}
		for _, namespace := range namespaces {
			if _, exists := m.caches[namespace]; exists {
				continue
			}
			cacheCtx, cancel := context.WithCancel(ctx)
			state := NewSandboxStateCache(m.client, namespace)
			state.SetInvalidationHandler(m.invalidate)
			m.caches[namespace] = state
			m.cancels[namespace] = cancel
			go state.Run(cacheCtx)
		}
		m.mu.Unlock()
	}
	refresh()
	ticker := time.NewTicker(sandboxRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			m.mu.Lock()
			for _, cancel := range m.cancels {
				cancel()
			}
			m.mu.Unlock()
			return
		case <-ticker.C:
			refresh()
		}
	}
}

func (m *MultiSandboxStateCache) SetInvalidationHandler(handler func(namespace, sandboxName, creationID string)) {
	m.mu.Lock()
	m.onInvalidate = handler
	for _, state := range m.caches {
		state.SetInvalidationHandler(m.invalidate)
	}
	m.mu.Unlock()
}

func (m *MultiSandboxStateCache) invalidate(namespace, sandboxName, creationID string) {
	m.mu.RLock()
	handler := m.onInvalidate
	m.mu.RUnlock()
	if handler != nil {
		handler(namespace, sandboxName, creationID)
	}
}

func (m *MultiSandboxStateCache) Allows(namespace, name, creationID string) bool {
	m.mu.RLock()
	state := m.caches[namespace]
	m.mu.RUnlock()
	return state != nil && state.Allows(namespace, name, creationID)
}

func (m *MultiSandboxStateCache) AllowsSandbox(namespace, name, uid, creationID string) bool {
	m.mu.RLock()
	state := m.caches[namespace]
	m.mu.RUnlock()
	return state != nil && state.AllowsSandbox(namespace, name, uid, creationID)
}

func (m *MultiSandboxStateCache) Ready() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, state := range m.caches {
		if state.Ready() {
			return true
		}
	}
	return false
}
