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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
)

const (
	labelWorkspaceKey = "sandboxes.buddy.io/workspace"
)

// WorkspaceReconciler reconciles a Workspace object.
type WorkspaceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=sandboxes.buddy.io,resources=workspaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sandboxes.buddy.io,resources=workspaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sandboxes.buddy.io,resources=workspaces/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete

func (r *WorkspaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var ws sandboxesv1alpha1.Workspace
	if err := r.Get(ctx, req.NamespacedName, &ws); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch Workspace")
		return ctrl.Result{}, err
	}

	if !ws.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &ws)
	}

	if updated, err := r.ensureRetentionFinalizer(ctx, &ws); updated || err != nil {
		return ctrl.Result{}, err
	}

	r.sweepStaleBoundSandbox(ctx, &ws)

	pvcName := ws.Status.PVCName
	if pvcName == "" {
		if ws.Spec.PersistentVolumeClaim != nil && ws.Spec.PersistentVolumeClaim.ClaimName != "" {
			pvcName = ws.Spec.PersistentVolumeClaim.ClaimName
		} else {
			pvcName = fmt.Sprintf("ws-%s-pvc", ws.Name)
		}
	}

	var pvc corev1.PersistentVolumeClaim
	err := r.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: pvcName}, &pvc)
	if err != nil && errors.IsNotFound(err) {
		return r.handleMissingPVC(ctx, &ws, pvcName)
	} else if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to get PVC %s: %w", pvcName, err)
	}

	r.syncPVCStatus(&ws, &pvc)
	return ctrl.Result{}, r.Status().Update(ctx, &ws)
}

func (r *WorkspaceReconciler) ensureRetentionFinalizer(ctx context.Context, ws *sandboxesv1alpha1.Workspace) (bool, error) {
	if ws.Spec.Size != "" && ws.Spec.PersistentVolumeClaim == nil {
		if !controllerutil.ContainsFinalizer(ws, workspaceRetentionFinalizer) {
			controllerutil.AddFinalizer(ws, workspaceRetentionFinalizer)
			return true, r.Update(ctx, ws)
		}
	}
	return false, nil
}

func (r *WorkspaceReconciler) sweepStaleBoundSandbox(ctx context.Context, ws *sandboxesv1alpha1.Workspace) {
	if ws.Status.BoundSandboxRef == nil || ws.Status.BoundSandboxRef.Name == "" {
		return
	}
	var sbx sandboxesv1alpha1.Sandbox
	sbxErr := r.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: ws.Status.BoundSandboxRef.Name}, &sbx)
	if errors.IsNotFound(sbxErr) || (sbxErr == nil && (sbx.Status.Phase == sandboxesv1alpha1.SandboxPhaseSucceeded || sbx.Status.Phase == sandboxesv1alpha1.SandboxPhaseFailed)) {
		ws.Status.BoundSandboxRef = nil
		if ws.Status.Phase == sandboxesv1alpha1.WorkspacePhaseBound {
			if ws.Status.PVCName != "" {
				var checkPVC corev1.PersistentVolumeClaim
				if pErr := r.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: ws.Status.PVCName}, &checkPVC); pErr == nil && checkPVC.Status.Phase == corev1.ClaimBound {
					ws.Status.Phase = sandboxesv1alpha1.WorkspacePhaseReady
				} else {
					ws.Status.Phase = sandboxesv1alpha1.WorkspacePhasePending
				}
			} else {
				ws.Status.Phase = sandboxesv1alpha1.WorkspacePhasePending
			}
		}
	}
}

func (r *WorkspaceReconciler) handleMissingPVC(ctx context.Context, ws *sandboxesv1alpha1.Workspace, pvcName string) (ctrl.Result, error) {
	if ws.Spec.PersistentVolumeClaim != nil && ws.Spec.PersistentVolumeClaim.ClaimName != "" {
		ws.Status.Phase = sandboxesv1alpha1.WorkspacePhaseFailed
		meta.SetStatusCondition(&ws.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "ExternalPVCNotFound",
			Message:            fmt.Sprintf("Referenced PVC %s not found in namespace", pvcName),
			LastTransitionTime: metav1.Now(),
		})
		return ctrl.Result{}, r.Status().Update(ctx, ws)
	}

	newPVC := r.buildPersistentVolumeClaim(ws, pvcName)
	if ws.Spec.ReclaimPolicy == sandboxesv1alpha1.WorkspaceReclaimDelete {
		if err := ctrl.SetControllerReference(ws, newPVC, r.Scheme); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to set controller reference on PVC: %w", err)
		}
	}
	if err := r.Create(ctx, newPVC); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create PVC %s: %w", pvcName, err)
	}
	ws.Status.PVCName = pvcName
	ws.Status.Phase = sandboxesv1alpha1.WorkspacePhasePending
	meta.SetStatusCondition(&ws.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "StoragePending",
		Message:            "PersistentVolumeClaim created, waiting for binding",
		LastTransitionTime: metav1.Now(),
	})
	return ctrl.Result{Requeue: true}, r.Status().Update(ctx, ws)
}

func (r *WorkspaceReconciler) syncPVCStatus(ws *sandboxesv1alpha1.Workspace, pvc *corev1.PersistentVolumeClaim) {
	ws.Status.PVCName = pvc.Name
	if qty, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
		ws.Status.Capacity = qty.String()
	}

	if pvc.Status.Phase == corev1.ClaimBound {
		if ws.Status.Phase != sandboxesv1alpha1.WorkspacePhaseBound {
			ws.Status.Phase = sandboxesv1alpha1.WorkspacePhaseReady
		}
		meta.SetStatusCondition(&ws.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "StorageBound",
			Message:            "Underlying PersistentVolumeClaim is bound and ready",
			LastTransitionTime: metav1.Now(),
		})
	} else {
		if ws.Status.Phase != sandboxesv1alpha1.WorkspacePhaseBound {
			ws.Status.Phase = sandboxesv1alpha1.WorkspacePhasePending
		}
		meta.SetStatusCondition(&ws.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "StoragePending",
			Message:            fmt.Sprintf("Underlying PersistentVolumeClaim phase is %s", pvc.Status.Phase),
			LastTransitionTime: metav1.Now(),
		})
	}
}

func (r *WorkspaceReconciler) reconcileDelete(ctx context.Context, ws *sandboxesv1alpha1.Workspace) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(ws, workspaceRetentionFinalizer) {
		if ws.Spec.ReclaimPolicy == sandboxesv1alpha1.WorkspaceReclaimDelete {
			pvcName := ws.Status.PVCName
			if pvcName == "" {
				pvcName = fmt.Sprintf("ws-%s-pvc", ws.Name)
			}
			var pvc corev1.PersistentVolumeClaim
			if err := r.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: pvcName}, &pvc); err == nil {
				if err := r.Delete(ctx, &pvc); err != nil && !errors.IsNotFound(err) {
					return ctrl.Result{}, fmt.Errorf("failed to delete PVC during workspace cleanup: %w", err)
				}
			}
		}
		controllerutil.RemoveFinalizer(ws, workspaceRetentionFinalizer)
		return ctrl.Result{}, r.Update(ctx, ws)
	}
	return ctrl.Result{}, nil
}

func (r *WorkspaceReconciler) buildPersistentVolumeClaim(ws *sandboxesv1alpha1.Workspace, name string) *corev1.PersistentVolumeClaim {
	sizeStr := ws.Spec.Size
	if sizeStr == "" {
		sizeStr = "10Gi"
	}
	reqSize, err := resource.ParseQuantity(sizeStr)
	if err != nil {
		reqSize = resource.MustParse("10Gi")
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ws.Namespace,
			Labels: map[string]string{
				labelManagedByKey: managedByValue,
				labelWorkspaceKey: ws.Name,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: reqSize,
				},
			},
			StorageClassName: ws.Spec.StorageClassName,
		},
	}
	return pvc
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sandboxesv1alpha1.Workspace{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Complete(r)
}
