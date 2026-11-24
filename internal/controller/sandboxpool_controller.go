/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
)

// SandboxPoolReconciler reconciles a SandboxPool object.
type SandboxPoolReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	inFlightMu        sync.Mutex
	inFlightCreations map[types.NamespacedName]map[string]time.Time
}

// +kubebuilder:rbac:groups=sandboxes.buddy.io,resources=sandboxpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sandboxes.buddy.io,resources=sandboxpools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sandboxes.buddy.io,resources=sandboxpools/finalizers,verbs=update
// +kubebuilder:rbac:groups=sandboxes.buddy.io,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sandboxes.buddy.io,resources=sandboxleases,verbs=get;list;watch

func (r *SandboxPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var pool sandboxesv1alpha1.SandboxPool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch SandboxPool")
		return ctrl.Result{}, err
	}

	if !pool.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// List child sandboxes belonging to this pool
	var sandboxList sandboxesv1alpha1.SandboxList
	if err := r.List(ctx, &sandboxList,
		client.InNamespace(pool.Namespace),
		client.MatchingLabels{sandboxesv1alpha1.LabelPoolName: poolLabelValue(pool.Name)},
	); err != nil {
		log.Error(err, "unable to list child sandboxes for pool")
		return ctrl.Result{}, err
	}

	readySandboxes, allocatedSandboxes, pendingInitSandboxes, terminalSandboxes := r.classifyRunners(sandboxList.Items)

	// Delete terminal runners immediately
	for _, termSbx := range terminalSandboxes {
		if err := r.Delete(ctx, &termSbx); err != nil && !errors.IsNotFound(err) {
			log.Error(err, "unable to delete terminal sandbox", "sandbox", termSbx.Name)
		}
	}

	now := time.Now()

	// Account for unobserved in-flight creations to prevent cache-lag over-provisioning
	poolKey := types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}
	unobservedCreations := r.getUnobservedCreations(poolKey, sandboxList.Items, now)

	// Scavenge expired claims that never bound
	activeAllocated, nextClaimRequeue, err := r.scavengeExpiredClaims(ctx, allocatedSandboxes, now)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Replenish standby runners to satisfy minWarm up to maxWarm
	totalActive, err := r.replenishStandbyRunners(ctx, &pool, readySandboxes, activeAllocated, pendingInitSandboxes, unobservedCreations, now)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Recycle surplus standby runners exceeding IdleTimeoutSeconds
	remainingReady, nextIdleRequeue := r.recycleSurplusStandbys(ctx, &pool, readySandboxes, now)
	totalActive -= len(readySandboxes) - len(remainingReady)

	requeueAfter := earliestRequeue(nextClaimRequeue, nextIdleRequeue)
	if unobservedCreations > 0 {
		cacheRequeue := 500 * time.Millisecond
		if requeueAfter == 0 || cacheRequeue < requeueAfter {
			requeueAfter = cacheRequeue
		}
	}

	// Update pool status
	return r.updatePoolStatus(ctx, &pool, len(remainingReady), len(activeAllocated), totalActive, requeueAfter)
}

func earliestRequeue(a, b time.Duration) time.Duration {
	if a <= 0 {
		return b
	}
	if b <= 0 {
		return a
	}
	if a < b {
		return a
	}
	return b
}

func (r *SandboxPoolReconciler) classifyRunners(items []sandboxesv1alpha1.Sandbox) (ready, allocated, pendingInit, terminal []sandboxesv1alpha1.Sandbox) {
	for _, sbx := range items {
		if !sbx.DeletionTimestamp.IsZero() {
			continue
		}

		if sbx.Status.Phase == sandboxesv1alpha1.SandboxPhaseFailed ||
			sbx.Status.Phase == sandboxesv1alpha1.SandboxPhaseSucceeded {
			terminal = append(terminal, sbx)
			continue
		}

		status := sbx.Labels[sandboxesv1alpha1.LabelPoolStatus]
		switch status {
		case sandboxesv1alpha1.PoolStatusClaimed, sandboxesv1alpha1.PoolStatusBound:
			allocated = append(allocated, sbx)
		case sandboxesv1alpha1.PoolStatusStandby:
			if sbx.Status.Phase == sandboxesv1alpha1.SandboxPhaseRunning {
				ready = append(ready, sbx)
			} else {
				pendingInit = append(pendingInit, sbx)
			}
		default:
			if sbx.Status.Phase == sandboxesv1alpha1.SandboxPhaseRunning {
				ready = append(ready, sbx)
			} else {
				pendingInit = append(pendingInit, sbx)
			}
		}
	}
	return ready, allocated, pendingInit, terminal
}

func (r *SandboxPoolReconciler) scavengeExpiredClaims(
	ctx context.Context,
	allocatedSandboxes []sandboxesv1alpha1.Sandbox,
	now time.Time,
) ([]sandboxesv1alpha1.Sandbox, time.Duration, error) {
	log := logf.FromContext(ctx)
	var nextRequeue time.Duration
	var activeAllocated []sandboxesv1alpha1.Sandbox
	var firstErr error

	for _, claimedSbx := range allocatedSandboxes {
		if claimedSbx.Labels[sandboxesv1alpha1.LabelPoolStatus] == sandboxesv1alpha1.PoolStatusBound {
			activeAllocated = append(activeAllocated, claimedSbx)
			continue
		}

		deadlineStr := claimedSbx.Annotations[sandboxesv1alpha1.AnnotationClaimDeadline]
		if deadlineStr == "" {
			activeAllocated = append(activeAllocated, claimedSbx)
			continue
		}
		deadlineTime, err := time.Parse(time.RFC3339, deadlineStr)
		if err != nil {
			activeAllocated = append(activeAllocated, claimedSbx)
			continue
		}

		leaseName := claimedSbx.Annotations[sandboxesv1alpha1.AnnotationLeaseName]
		if leaseName == "" {
			leaseName = claimedSbx.Labels[sandboxesv1alpha1.LabelLeaseName]
		}
		var lease sandboxesv1alpha1.SandboxLease
		isBound := false
		if leaseName != "" {
			if err := r.Get(ctx, types.NamespacedName{Namespace: claimedSbx.Namespace, Name: leaseName}, &lease); err != nil {
				if !errors.IsNotFound(err) {
					log.Error(err, "failed to get lease for claimed sandbox, retaining runner", "lease", leaseName, "sandbox", claimedSbx.Name)
					activeAllocated = append(activeAllocated, claimedSbx)
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
			} else {
				if lease.Status.Phase == sandboxesv1alpha1.SandboxLeasePhaseBound &&
					lease.Status.BoundSandboxRef != nil &&
					lease.Status.BoundSandboxRef.Name == claimedSbx.Name {
					isBound = true
				}
			}
		}
		if isBound {
			activeAllocated = append(activeAllocated, claimedSbx)
			continue
		}

		if !now.Before(deadlineTime) {
			log.Info("scavenging expired unconfirmed claimed sandbox", "sandbox", claimedSbx.Name)
			var deleteOpts []client.DeleteOption
			if claimedSbx.ResourceVersion != "" {
				deleteOpts = append(deleteOpts, client.Preconditions(metav1.Preconditions{ResourceVersion: &claimedSbx.ResourceVersion}))
			}
			if err := r.Delete(ctx, &claimedSbx, deleteOpts...); err != nil {
				if errors.IsConflict(err) {
					log.Info("conflict scavenging claimed sandbox due to concurrent confirmation, retaining runner", "sandbox", claimedSbx.Name)
					activeAllocated = append(activeAllocated, claimedSbx)
					continue
				}
				if !errors.IsNotFound(err) {
					log.Error(err, "failed to delete expired claimed sandbox, retaining in capacity count", "sandbox", claimedSbx.Name)
					activeAllocated = append(activeAllocated, claimedSbx)
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
			}
		} else {
			rem := deadlineTime.Sub(now)
			if rem > 0 && (nextRequeue == 0 || rem < nextRequeue) {
				nextRequeue = rem
			}
			activeAllocated = append(activeAllocated, claimedSbx)
		}
	}

	return activeAllocated, nextRequeue, firstErr
}

func (r *SandboxPoolReconciler) replenishStandbyRunners(
	ctx context.Context,
	pool *sandboxesv1alpha1.SandboxPool,
	ready, allocated, pendingInit []sandboxesv1alpha1.Sandbox,
	unobservedCreations int,
	now time.Time,
) (int, error) {
	log := logf.FromContext(ctx)
	totalActive := len(ready) + len(allocated) + len(pendingInit) + unobservedCreations
	standbyCount := len(ready) + len(pendingInit) + unobservedCreations
	deficit := int(pool.Spec.MinWarm) - standbyCount
	capacityLeft := int(pool.Spec.MaxWarm) - totalActive

	allowedNew := deficit
	if allowedNew > capacityLeft {
		allowedNew = capacityLeft
	}

	poolKey := types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}
	for i := 0; i < allowedNew; i++ {
		newSbx := r.buildStandbySandbox(pool)
		if err := r.Create(ctx, newSbx); err != nil {
			log.Error(err, "unable to create replacement standby sandbox")
			return totalActive, err
		}
		r.recordInFlightCreation(poolKey, newSbx.Name, now)
		log.Info("created standby sandbox", "sandbox", newSbx.Name, "pool", pool.Name)
		totalActive++
	}

	return totalActive, nil
}

func (r *SandboxPoolReconciler) recycleSurplusStandbys(
	ctx context.Context,
	pool *sandboxesv1alpha1.SandboxPool,
	readySandboxes []sandboxesv1alpha1.Sandbox,
	now time.Time,
) ([]sandboxesv1alpha1.Sandbox, time.Duration) {
	log := logf.FromContext(ctx)
	if pool.Spec.IdleTimeoutSeconds <= 0 || len(readySandboxes) <= int(pool.Spec.MinWarm) {
		return readySandboxes, 0
	}

	sort.Slice(readySandboxes, func(i, j int) bool {
		return readySandboxes[i].CreationTimestamp.Before(&readySandboxes[j].CreationTimestamp)
	})
	surplus := len(readySandboxes) - int(pool.Spec.MinWarm)
	idleTimeout := time.Duration(pool.Spec.IdleTimeoutSeconds) * time.Second
	var nextRequeue time.Duration
	var remaining []sandboxesv1alpha1.Sandbox

	for i := 0; i < len(readySandboxes); i++ {
		sbx := readySandboxes[i]
		if i < surplus {
			idleAge := now.Sub(sbx.CreationTimestamp.Time)
			if idleAge >= idleTimeout {
				log.Info("recycling surplus idle standby runner", "sandbox", sbx.Name, "idleAge", idleAge)
				var deleteOpts []client.DeleteOption
				if sbx.ResourceVersion != "" {
					deleteOpts = append(deleteOpts, client.Preconditions(metav1.Preconditions{ResourceVersion: &sbx.ResourceVersion}))
				}
				if err := r.Delete(ctx, &sbx, deleteOpts...); err != nil {
					if errors.IsConflict(err) {
						log.Info("conflict deleting surplus standby runner due to concurrent modification", "sandbox", sbx.Name)
						remaining = append(remaining, sbx)
						continue
					}
					if !errors.IsNotFound(err) {
						log.Error(err, "unable to delete surplus standby runner", "sandbox", sbx.Name)
					}
					remaining = append(remaining, sbx)
					continue
				}
				continue
			}

			rem := idleTimeout - idleAge
			if rem > 0 && (nextRequeue == 0 || rem < nextRequeue) {
				nextRequeue = rem
			}
		}
		remaining = append(remaining, sbx)
	}

	return remaining, nextRequeue
}

func (r *SandboxPoolReconciler) updatePoolStatus(
	ctx context.Context,
	pool *sandboxesv1alpha1.SandboxPool,
	readyCount, allocatedCount, totalCount int,
	requeueAfter time.Duration,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	pool.Status.ReadyReplicas = int32(readyCount)
	pool.Status.AllocatedReplicas = int32(allocatedCount)
	pool.Status.TotalReplicas = int32(totalCount)

	conditionStatus := metav1.ConditionTrue
	conditionReason := "PoolReady"
	conditionMessage := fmt.Sprintf("Pool has %d ready and %d allocated runners", pool.Status.ReadyReplicas, pool.Status.AllocatedReplicas)
	if pool.Status.ReadyReplicas < pool.Spec.MinWarm {
		conditionReason = sandboxesv1alpha1.ConditionPoolScaling
		conditionMessage = fmt.Sprintf("Pool is scaling to satisfy minWarm %d (currently %d ready)", pool.Spec.MinWarm, pool.Status.ReadyReplicas)
	}

	meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               sandboxesv1alpha1.ConditionPoolReady,
		Status:             conditionStatus,
		Reason:             conditionReason,
		Message:            conditionMessage,
		LastTransitionTime: metav1.Now(),
	})

	if err := r.Status().Update(ctx, pool); err != nil {
		log.Error(err, "unable to update SandboxPool status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *SandboxPoolReconciler) buildStandbySandbox(pool *sandboxesv1alpha1.SandboxPool) *sandboxesv1alpha1.Sandbox {
	name := generateRunnerName(pool.Name)
	boolTrue := true

	cmd := pool.Spec.Template.Command
	if len(cmd) == 0 {
		cmd = []string{"sleep", "infinity"}
	}

	workingDir := pool.Spec.Template.WorkingDir
	if workingDir == "" {
		workingDir = "/workspace"
	}

	sbx := &sandboxesv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: pool.Namespace,
			Labels: map[string]string{
				sandboxesv1alpha1.LabelPoolName:   poolLabelValue(pool.Name),
				sandboxesv1alpha1.LabelPoolStatus: sandboxesv1alpha1.PoolStatusStandby,
			},
		},
		Spec: sandboxesv1alpha1.SandboxSpec{
			Image:            pool.Spec.Template.Image,
			Command:          cmd,
			Args:             pool.Spec.Template.Args,
			WorkingDir:       workingDir,
			RuntimeClassName: pool.Spec.Template.RuntimeClassName,
			Resources:        pool.Spec.Template.Resources,
			Env:              pool.Spec.Template.Env,
			Workspace: sandboxesv1alpha1.WorkspaceSpec{
				MountPath: workingDir,
				Ephemeral: &boolTrue,
			},
			NetworkProfileRef: pool.Spec.Template.NetworkProfileRef,
		},
	}

	_ = ctrl.SetControllerReference(pool, sbx, r.Scheme)
	return sbx
}

func poolLabelValue(poolName string) string {
	if len(poolName) <= 63 {
		return poolName
	}
	h := sha256.Sum256([]byte(poolName))
	hashStr := hex.EncodeToString(h[:8]) // 16 hex chars
	trimmed := strings.TrimRight(poolName[:46], "-.")
	return fmt.Sprintf("%s-%s", trimmed, hashStr)
}

func generateRunnerName(poolName string) string {
	suffix := rand.String(5)
	if len(poolName) <= 57 {
		trimmed := strings.TrimRight(poolName, "-.")
		return fmt.Sprintf("%s-%s", trimmed, suffix)
	}
	h := sha256.Sum256([]byte(poolName))
	hashStr := hex.EncodeToString(h[:4]) // 8 hex chars
	trimmed := strings.TrimRight(poolName[:48], "-.")
	return fmt.Sprintf("%s-%s-%s", trimmed, hashStr, suffix)
}

func (r *SandboxPoolReconciler) getUnobservedCreations(
	poolKey types.NamespacedName,
	observed []sandboxesv1alpha1.Sandbox,
	now time.Time,
) int {
	r.inFlightMu.Lock()
	defer r.inFlightMu.Unlock()

	if r.inFlightCreations == nil {
		r.inFlightCreations = make(map[types.NamespacedName]map[string]time.Time)
		return 0
	}

	creations := r.inFlightCreations[poolKey]
	if len(creations) == 0 {
		return 0
	}

	observedSet := make(map[string]struct{}, len(observed))
	for _, sbx := range observed {
		observedSet[sbx.Name] = struct{}{}
	}

	const creationTTL = 15 * time.Second
	for name, createdAt := range creations {
		if _, exists := observedSet[name]; exists || now.Sub(createdAt) > creationTTL {
			delete(creations, name)
		}
	}

	if len(creations) == 0 {
		delete(r.inFlightCreations, poolKey)
		return 0
	}

	return len(creations)
}

func (r *SandboxPoolReconciler) recordInFlightCreation(
	poolKey types.NamespacedName,
	sandboxName string,
	now time.Time,
) {
	r.inFlightMu.Lock()
	defer r.inFlightMu.Unlock()

	if r.inFlightCreations == nil {
		r.inFlightCreations = make(map[types.NamespacedName]map[string]time.Time)
	}
	if r.inFlightCreations[poolKey] == nil {
		r.inFlightCreations[poolKey] = make(map[string]time.Time)
	}
	r.inFlightCreations[poolKey][sandboxName] = now
}

func (r *SandboxPoolReconciler) findPoolsForLease(ctx context.Context, obj client.Object) []ctrl.Request {
	lease, ok := obj.(*sandboxesv1alpha1.SandboxLease)
	if !ok || lease.Spec.PoolRef.Name == "" {
		return nil
	}
	return []ctrl.Request{
		{
			NamespacedName: types.NamespacedName{
				Namespace: lease.Namespace,
				Name:      lease.Spec.PoolRef.Name,
			},
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *SandboxPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sandboxesv1alpha1.SandboxPool{}).
		Owns(&sandboxesv1alpha1.Sandbox{}).
		Watches(&sandboxesv1alpha1.SandboxLease{}, handler.EnqueueRequestsFromMapFunc(r.findPoolsForLease)).
		Named("sandboxpool").
		Complete(r)
}
