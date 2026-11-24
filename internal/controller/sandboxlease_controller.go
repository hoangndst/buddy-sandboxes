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
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
)

// SandboxLeaseReconciler reconciles a SandboxLease object.
type SandboxLeaseReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=sandboxes.buddy.io,resources=sandboxleases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sandboxes.buddy.io,resources=sandboxleases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sandboxes.buddy.io,resources=sandboxleases/finalizers,verbs=update
// +kubebuilder:rbac:groups=sandboxes.buddy.io,resources=sandboxpools,verbs=get;list;watch
// +kubebuilder:rbac:groups=sandboxes.buddy.io,resources=sandboxes,verbs=get;list;watch;update;patch;delete

func (r *SandboxLeaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var lease sandboxesv1alpha1.SandboxLease
	if err := r.Get(ctx, req.NamespacedName, &lease); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch SandboxLease")
		return ctrl.Result{}, err
	}

	// Handle Deletion
	if !lease.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &lease)
	}

	// Ensure finalizer
	if !controllerutil.ContainsFinalizer(&lease, sandboxesv1alpha1.LeaseProtectionFinalizer) {
		controllerutil.AddFinalizer(&lease, sandboxesv1alpha1.LeaseProtectionFinalizer)
		if err := r.Update(ctx, &lease); err != nil {
			log.Error(err, "unable to add finalizer to SandboxLease")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Handle Explicit Client Release
	if lease.Spec.Released {
		return r.handleRelease(ctx, &lease)
	}

	// Handle Bound State
	if lease.Status.Phase == sandboxesv1alpha1.SandboxLeasePhaseBound {
		return r.handleBoundLease(ctx, &lease)
	}

	// Terminal states check
	if lease.Status.Phase == sandboxesv1alpha1.SandboxLeasePhaseReleased ||
		lease.Status.Phase == sandboxesv1alpha1.SandboxLeasePhaseExpired ||
		lease.Status.Phase == sandboxesv1alpha1.SandboxLeasePhaseFailed {
		return ctrl.Result{}, nil
	}

	// Handle Pending / Initial State
	return r.handlePendingLease(ctx, &lease)
}

func (r *SandboxLeaseReconciler) deleteBoundSandbox(ctx context.Context, namespace, name, action string) error {
	log := logf.FromContext(ctx)
	var sbx sandboxesv1alpha1.Sandbox
	err := r.Get(ctx, types.NamespacedName{
		Namespace: namespace,
		Name:      name,
	}, &sbx)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		log.Error(err, "unable to fetch bound sandbox", "sandbox", name, "action", action)
		return err
	}

	log.Info("deleting claimed sandbox", "sandbox", sbx.Name, "action", action)
	if err := r.Delete(ctx, &sbx); err != nil && !errors.IsNotFound(err) {
		log.Error(err, "unable to delete bound sandbox", "sandbox", sbx.Name, "action", action)
		return err
	}
	return nil
}

func (r *SandboxLeaseReconciler) reconcileDelete(ctx context.Context, lease *sandboxesv1alpha1.SandboxLease) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if controllerutil.ContainsFinalizer(lease, sandboxesv1alpha1.LeaseProtectionFinalizer) {
		if lease.Status.BoundSandboxRef != nil && lease.Status.BoundSandboxRef.Name != "" {
			if err := r.deleteBoundSandbox(ctx, lease.Namespace, lease.Status.BoundSandboxRef.Name, "deletion"); err != nil {
				return ctrl.Result{}, err
			}
		}

		controllerutil.RemoveFinalizer(lease, sandboxesv1alpha1.LeaseProtectionFinalizer)
		if err := r.Update(ctx, lease); err != nil {
			log.Error(err, "unable to remove finalizer from SandboxLease")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *SandboxLeaseReconciler) handleRelease(ctx context.Context, lease *sandboxesv1alpha1.SandboxLease) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if lease.Status.Phase == sandboxesv1alpha1.SandboxLeasePhaseReleased {
		return ctrl.Result{}, nil
	}

	if lease.Status.BoundSandboxRef != nil && lease.Status.BoundSandboxRef.Name != "" {
		if err := r.deleteBoundSandbox(ctx, lease.Namespace, lease.Status.BoundSandboxRef.Name, "release"); err != nil {
			return ctrl.Result{}, err
		}
	}

	lease.Status.Phase = sandboxesv1alpha1.SandboxLeasePhaseReleased
	meta.SetStatusCondition(&lease.Status.Conditions, metav1.Condition{
		Type:               sandboxesv1alpha1.ConditionLeaseBound,
		Status:             metav1.ConditionFalse,
		Reason:             "Released",
		Message:            "Client completed execution and released lease",
		LastTransitionTime: metav1.Now(),
	})

	if err := r.Status().Update(ctx, lease); err != nil {
		log.Error(err, "unable to update lease status to Released")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *SandboxLeaseReconciler) handleBoundLease(ctx context.Context, lease *sandboxesv1alpha1.SandboxLease) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	now := time.Now()

	// Check TTL Expiration
	if lease.Status.ExpiresAt != nil && now.After(lease.Status.ExpiresAt.Time) {
		log.Info("lease expired, deleting claimed runner", "lease", lease.Name)
		if lease.Status.BoundSandboxRef != nil && lease.Status.BoundSandboxRef.Name != "" {
			if err := r.deleteBoundSandbox(ctx, lease.Namespace, lease.Status.BoundSandboxRef.Name, "expiration"); err != nil {
				return ctrl.Result{}, err
			}
		}

		lease.Status.Phase = sandboxesv1alpha1.SandboxLeasePhaseExpired
		meta.SetStatusCondition(&lease.Status.Conditions, metav1.Condition{
			Type:               sandboxesv1alpha1.ConditionLeaseExpired,
			Status:             metav1.ConditionTrue,
			Reason:             "TimeoutExpired",
			Message:            fmt.Sprintf("Lease exceeded duration of %d seconds", lease.Spec.TimeoutSeconds),
			LastTransitionTime: metav1.Now(),
		})
		meta.SetStatusCondition(&lease.Status.Conditions, metav1.Condition{
			Type:               sandboxesv1alpha1.ConditionLeaseBound,
			Status:             metav1.ConditionFalse,
			Reason:             "Expired",
			Message:            "Lease has expired",
			LastTransitionTime: metav1.Now(),
		})

		if err := r.Status().Update(ctx, lease); err != nil {
			log.Error(err, "unable to update lease status to Expired")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Check bound child Sandbox health
	if lease.Status.BoundSandboxRef != nil && lease.Status.BoundSandboxRef.Name != "" {
		var sbx sandboxesv1alpha1.Sandbox
		err := r.Get(ctx, types.NamespacedName{
			Namespace: lease.Namespace,
			Name:      lease.Status.BoundSandboxRef.Name,
		}, &sbx)
		if errors.IsNotFound(err) || (err == nil && (sbx.Status.Phase == sandboxesv1alpha1.SandboxPhaseFailed || sbx.Status.Phase == sandboxesv1alpha1.SandboxPhaseSucceeded)) {
			log.Info("bound runner terminated prematurely", "lease", lease.Name, "sandbox", lease.Status.BoundSandboxRef.Name)
			lease.Status.Phase = sandboxesv1alpha1.SandboxLeasePhaseFailed
			meta.SetStatusCondition(&lease.Status.Conditions, metav1.Condition{
				Type:               sandboxesv1alpha1.ConditionLeaseBound,
				Status:             metav1.ConditionFalse,
				Reason:             "RunnerTerminated",
				Message:            "Claimed runner pod terminated or exited",
				LastTransitionTime: metav1.Now(),
			})
			if err := r.Status().Update(ctx, lease); err != nil {
				log.Error(err, "unable to update lease status to Failed")
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	}

	// Schedule next requeue for expiration
	requeueAfter := time.Until(lease.Status.ExpiresAt.Time)
	if requeueAfter < 0 {
		requeueAfter = 0
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *SandboxLeaseReconciler) handlePendingLease(ctx context.Context, lease *sandboxesv1alpha1.SandboxLease) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Check Queue Timeout
	queueTimeout := lease.Spec.QueueTimeoutSeconds
	if queueTimeout <= 0 {
		queueTimeout = 60
	}
	if time.Since(lease.CreationTimestamp.Time) > time.Duration(queueTimeout)*time.Second {
		log.Info("lease queue timeout exceeded waiting for standby runner", "lease", lease.Name)
		lease.Status.Phase = sandboxesv1alpha1.SandboxLeasePhaseFailed
		meta.SetStatusCondition(&lease.Status.Conditions, metav1.Condition{
			Type:               sandboxesv1alpha1.ConditionLeaseBound,
			Status:             metav1.ConditionFalse,
			Reason:             "QueueTimeoutExceeded",
			Message:            fmt.Sprintf("Timed out after %d seconds waiting for standby runner", queueTimeout),
			LastTransitionTime: metav1.Now(),
		})
		if err := r.Status().Update(ctx, lease); err != nil {
			log.Error(err, "unable to update lease status to Failed")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// List candidate standby sandboxes
	var sbxList sandboxesv1alpha1.SandboxList
	if err := r.List(ctx, &sbxList,
		client.InNamespace(lease.Namespace),
		client.MatchingLabels{
			sandboxesv1alpha1.LabelPoolName:   lease.Spec.PoolRef.Name,
			sandboxesv1alpha1.LabelPoolStatus: sandboxesv1alpha1.PoolStatusStandby,
		},
	); err != nil {
		log.Error(err, "unable to list standby sandboxes for lease claim")
		return ctrl.Result{}, err
	}

	var candidates []sandboxesv1alpha1.Sandbox
	for _, sbx := range sbxList.Items {
		if sbx.DeletionTimestamp.IsZero() && sbx.Status.Phase == sandboxesv1alpha1.SandboxPhaseRunning {
			candidates = append(candidates, sbx)
		}
	}

	if len(candidates) == 0 {
		// No ready standby runners available
		if lease.Status.Phase != sandboxesv1alpha1.SandboxLeasePhasePending {
			lease.Status.Phase = sandboxesv1alpha1.SandboxLeasePhasePending
			meta.SetStatusCondition(&lease.Status.Conditions, metav1.Condition{
				Type:               sandboxesv1alpha1.ConditionLeaseBound,
				Status:             metav1.ConditionFalse,
				Reason:             "PendingStandbyRunner",
				Message:            fmt.Sprintf("Awaiting standby runner from pool %s", lease.Spec.PoolRef.Name),
				LastTransitionTime: metav1.Now(),
			})
			_ = r.Status().Update(ctx, lease)
		}

		remaining := time.Duration(queueTimeout)*time.Second - time.Since(lease.CreationTimestamp.Time)
		if remaining > 2*time.Second {
			remaining = 2 * time.Second
		}
		return ctrl.Result{RequeueAfter: remaining}, nil
	}

	// Sort candidates by creation timestamp for deterministic FIFO allocation
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].CreationTimestamp.Before(&candidates[j].CreationTimestamp)
	})

	// Attempt atomic CAS claim
	for i := range candidates {
		candidate := &candidates[i]
		orig := candidate.DeepCopy()

		if candidate.Labels == nil {
			candidate.Labels = make(map[string]string)
		}
		candidate.Labels[sandboxesv1alpha1.LabelPoolStatus] = sandboxesv1alpha1.PoolStatusClaimed
		candidate.Labels[sandboxesv1alpha1.LabelLeaseName] = lease.Name

		if candidate.Annotations == nil {
			candidate.Annotations = make(map[string]string)
		}
		now := metav1.Now()
		deadline := metav1.NewTime(now.Add(15 * time.Second))
		candidate.Annotations[sandboxesv1alpha1.AnnotationClaimedAt] = now.Format(time.RFC3339)
		candidate.Annotations[sandboxesv1alpha1.AnnotationClaimDeadline] = deadline.Format(time.RFC3339)

		if err := r.Patch(ctx, candidate, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
			if errors.IsConflict(err) {
				continue // candidate claimed by concurrent lease, try next candidate
			}
			log.Error(err, "error patching candidate sandbox during claim")
			return ctrl.Result{}, err
		}

		// CAS Claim Succeeded!
		leasedAt := metav1.Now()
		timeout := lease.Spec.TimeoutSeconds
		if timeout <= 0 {
			timeout = 300
		}
		expiresAt := metav1.NewTime(leasedAt.Add(time.Duration(timeout) * time.Second))

		podName := candidate.Status.PodName
		if podName == "" {
			podName = fmt.Sprintf("%s-sandbox", candidate.Name)
		}

		lease.Status.Phase = sandboxesv1alpha1.SandboxLeasePhaseBound
		lease.Status.BoundSandboxRef = &corev1.LocalObjectReference{Name: candidate.Name}
		lease.Status.BoundPodName = podName
		lease.Status.LeasedAt = &leasedAt
		lease.Status.ExpiresAt = &expiresAt

		meta.SetStatusCondition(&lease.Status.Conditions, metav1.Condition{
			Type:               sandboxesv1alpha1.ConditionLeaseBound,
			Status:             metav1.ConditionTrue,
			Reason:             "RunnerBound",
			Message:            fmt.Sprintf("Bound runner %s successfully", candidate.Name),
			LastTransitionTime: leasedAt,
		})

		if err := r.Status().Update(ctx, lease); err != nil {
			log.Error(err, "unable to update lease status to Bound")
			return ctrl.Result{}, err
		}

		// Atomically transition the claimed sandbox to Bound and clear the reservation deadline.
		// If scavenging already deleted the runner or concurrent modification occurred, abort and retry.
		confirmOrig := candidate.DeepCopy()
		candidate.Labels[sandboxesv1alpha1.LabelPoolStatus] = sandboxesv1alpha1.PoolStatusBound
		delete(candidate.Annotations, sandboxesv1alpha1.AnnotationClaimDeadline)
		if err := r.Patch(ctx, candidate, client.MergeFromWithOptions(confirmOrig, client.MergeFromWithOptimisticLock{})); err != nil {
			if errors.IsConflict(err) || errors.IsNotFound(err) {
				log.Info("claim confirmation collided with scavenger or concurrent modification, reverting lease and retrying", "sandbox", candidate.Name)
				lease.Status.Phase = sandboxesv1alpha1.SandboxLeasePhasePending
				lease.Status.BoundSandboxRef = nil
				lease.Status.BoundPodName = ""
				_ = r.Status().Update(ctx, lease)
				return ctrl.Result{Requeue: true}, nil
			}
			log.Error(err, "error patching sandbox to Bound during claim confirmation")
			return ctrl.Result{}, err
		}

		log.Info("successfully bound runner to lease", "lease", lease.Name, "sandbox", candidate.Name)
		return ctrl.Result{RequeueAfter: time.Until(expiresAt.Time)}, nil
	}

	// All candidates had conflict, requeue immediately to re-evaluate
	return ctrl.Result{Requeue: true}, nil
}

func (r *SandboxLeaseReconciler) findLeasesForSandbox(ctx context.Context, obj client.Object) []ctrl.Request {
	sbx, ok := obj.(*sandboxesv1alpha1.Sandbox)
	if !ok {
		return nil
	}

	poolName := sbx.Labels[sandboxesv1alpha1.LabelPoolName]
	status := sbx.Labels[sandboxesv1alpha1.LabelPoolStatus]
	leaseName := sbx.Labels[sandboxesv1alpha1.LabelLeaseName]

	var reqs []ctrl.Request

	// If this is a claimed or bound sandbox and names a lease, wake up that specific lease
	if (status == sandboxesv1alpha1.PoolStatusClaimed || status == sandboxesv1alpha1.PoolStatusBound) && leaseName != "" {
		reqs = append(reqs, ctrl.Request{
			NamespacedName: types.NamespacedName{
				Namespace: sbx.Namespace,
				Name:      leaseName,
			},
		})
		return reqs
	}

	// If this is a standby sandbox that became Running, wake up pending leases for this pool
	if status == sandboxesv1alpha1.PoolStatusStandby && poolName != "" &&
		sbx.Status.Phase == sandboxesv1alpha1.SandboxPhaseRunning {
		var leaseList sandboxesv1alpha1.SandboxLeaseList
		if err := r.List(ctx, &leaseList, client.InNamespace(sbx.Namespace)); err == nil {
			for _, l := range leaseList.Items {
				if l.Spec.PoolRef.Name == poolName &&
					(l.Status.Phase == "" || l.Status.Phase == sandboxesv1alpha1.SandboxLeasePhasePending) {
					reqs = append(reqs, ctrl.Request{
						NamespacedName: types.NamespacedName{
							Namespace: l.Namespace,
							Name:      l.Name,
						},
					})
				}
			}
		}
	}

	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *SandboxLeaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sandboxesv1alpha1.SandboxLease{}).
		Watches(&sandboxesv1alpha1.Sandbox{}, handler.EnqueueRequestsFromMapFunc(r.findLeasesForSandbox)).
		Named("sandboxlease").
		Complete(r)
}
