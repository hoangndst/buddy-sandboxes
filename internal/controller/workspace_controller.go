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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
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

	r.syncPVCStatus(ctx, &ws, &pvc)
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
	if errors.IsNotFound(sbxErr) {
		r.clearBoundSandbox(ctx, ws)
		return
	}
	if sbxErr == nil && (sbx.Status.Phase == sandboxesv1alpha1.SandboxPhaseSucceeded || sbx.Status.Phase == sandboxesv1alpha1.SandboxPhaseFailed) {
		podName := sbx.Status.PodName
		if podName == "" {
			podName = fmt.Sprintf("%s-sandbox", sbx.Name)
		}
		var pod corev1.Pod
		if err := r.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: podName}, &pod); err != nil {
			if errors.IsNotFound(err) {
				r.clearBoundSandbox(ctx, ws)
			}
		} else if isTerminalPod(&pod) && pod.DeletionTimestamp == nil {
			r.clearBoundSandbox(ctx, ws)
		}
	}
}

func (r *WorkspaceReconciler) clearBoundSandbox(ctx context.Context, ws *sandboxesv1alpha1.Workspace) {
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

func (r *WorkspaceReconciler) syncPVCStatus(ctx context.Context, ws *sandboxesv1alpha1.Workspace, pvc *corev1.PersistentVolumeClaim) {
	ws.Status.PVCName = pvc.Name
	if qty, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
		ws.Status.Capacity = qty.String()
	}

	if ws.Spec.ReclaimPolicy == sandboxesv1alpha1.WorkspaceReclaimRetain {
		if removeWorkspaceOwnerReference(pvc, ws) {
			_ = r.Update(ctx, pvc)
		}
	} else if ws.Spec.ReclaimPolicy == sandboxesv1alpha1.WorkspaceReclaimDelete {
		if !hasWorkspaceOwnerReference(pvc, ws) {
			if err := ctrl.SetControllerReference(ws, pvc, r.Scheme); err == nil {
				_ = r.Update(ctx, pvc)
			}
		}
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

func removeWorkspaceOwnerReference(obj metav1.Object, ws *sandboxesv1alpha1.Workspace) bool {
	existingRefs := obj.GetOwnerReferences()
	if len(existingRefs) == 0 {
		return false
	}
	newRefs := make([]metav1.OwnerReference, 0, len(existingRefs))
	removed := false
	for _, ref := range existingRefs {
		if (ref.UID != "" && ref.UID == ws.UID) || (ref.Kind == "Workspace" && ref.Name == ws.Name) {
			removed = true
			continue
		}
		newRefs = append(newRefs, ref)
	}
	if removed {
		obj.SetOwnerReferences(newRefs)
	}
	return removed
}

func hasWorkspaceOwnerReference(obj metav1.Object, ws *sandboxesv1alpha1.Workspace) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if (ref.UID != "" && ref.UID == ws.UID) || (ref.Kind == "Workspace" && ref.Name == ws.Name) {
			return true
		}
	}
	return false
}

func (r *WorkspaceReconciler) reconcileDelete(ctx context.Context, ws *sandboxesv1alpha1.Workspace) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(ws, workspaceRetentionFinalizer) {
		pvcName := ws.Status.PVCName
		if pvcName == "" {
			if ws.Spec.PersistentVolumeClaim != nil && ws.Spec.PersistentVolumeClaim.ClaimName != "" {
				pvcName = ws.Spec.PersistentVolumeClaim.ClaimName
			} else {
				pvcName = fmt.Sprintf("ws-%s-pvc", ws.Name)
			}
		}
		var pvc corev1.PersistentVolumeClaim
		pvcFound := false
		if err := r.Get(ctx, client.ObjectKey{Namespace: ws.Namespace, Name: pvcName}, &pvc); err == nil {
			pvcFound = true
		}
		if ws.Spec.ReclaimPolicy == sandboxesv1alpha1.WorkspaceReclaimDelete {
			if pvcFound {
				if err := r.Delete(ctx, &pvc); err != nil && !errors.IsNotFound(err) {
					return ctrl.Result{}, fmt.Errorf("failed to delete PVC during workspace cleanup: %w", err)
				}
			}
		} else if ws.Spec.ReclaimPolicy == sandboxesv1alpha1.WorkspaceReclaimRetain {
			if pvcFound && removeWorkspaceOwnerReference(&pvc, ws) {
				if err := r.Update(ctx, &pvc); err != nil && !errors.IsNotFound(err) {
					return ctrl.Result{}, fmt.Errorf("failed to strip owner reference on retained PVC: %w", err)
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

func (r *WorkspaceReconciler) findWorkspacesForPVC(ctx context.Context, obj client.Object) []ctrl.Request {
	pvc, ok := obj.(*corev1.PersistentVolumeClaim)
	if !ok {
		return nil
	}
	var workspaces sandboxesv1alpha1.WorkspaceList
	if err := r.List(ctx, &workspaces, client.InNamespace(pvc.Namespace)); err != nil {
		return nil
	}
	var reqs []ctrl.Request
	for _, ws := range workspaces.Items {
		matches := false
		if ws.Spec.PersistentVolumeClaim != nil && ws.Spec.PersistentVolumeClaim.ClaimName == pvc.Name {
			matches = true
		} else if ws.Status.PVCName == pvc.Name {
			matches = true
		} else if pvc.Name == fmt.Sprintf("pvc-for-%s", ws.Name) {
			matches = true
		}
		if matches {
			reqs = append(reqs, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Namespace: ws.Namespace,
					Name:      ws.Name,
				},
			})
		}
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sandboxesv1alpha1.Workspace{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Watches(&corev1.PersistentVolumeClaim{}, handler.EnqueueRequestsFromMapFunc(r.findWorkspacesForPVC)).
		Named("workspace").
		Complete(r)
}
