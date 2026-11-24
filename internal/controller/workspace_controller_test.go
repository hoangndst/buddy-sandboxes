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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
)

var _ = Describe("Workspace Controller", func() {
	const namespace = "default"

	Context("When reconciling a Workspace", func() {
		It("should provision dynamic PVC, manage finalizers, and transition phases", func() {
			wsName := "test-workspace-dynamic"
			wsKey := types.NamespacedName{Name: wsName, Namespace: namespace}
			pvcKey := types.NamespacedName{Name: "ws-" + wsName + "-pvc", Namespace: namespace}

			workspace := &sandboxesv1alpha1.Workspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:      wsName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.WorkspaceSpec{
					Size:          "5Gi",
					ReclaimPolicy: sandboxesv1alpha1.WorkspaceReclaimRetain,
				},
			}
			Expect(k8sClient.Create(ctx, workspace)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, workspace) }()

			reconciler := &WorkspaceReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// First reconcile: adds finalizer
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile: creates PVC
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
			Expect(err).NotTo(HaveOccurred())

			// Verify PVC was created
			var pvc corev1.PersistentVolumeClaim
			Expect(k8sClient.Get(ctx, pvcKey, &pvc)).To(Succeed())
			Expect(pvc.Labels[labelWorkspaceKey]).To(Equal(wsName))
			Expect(pvc.Labels[labelManagedByKey]).To(Equal(managedByValue))

			// Verify Workspace status is Pending
			var updatedWS sandboxesv1alpha1.Workspace
			Expect(k8sClient.Get(ctx, wsKey, &updatedWS)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(&updatedWS, workspaceRetentionFinalizer)).To(BeTrue())
			Expect(updatedWS.Status.PVCName).To(Equal(pvc.Name))
			Expect(updatedWS.Status.Phase).To(Equal(sandboxesv1alpha1.WorkspacePhasePending))

			// Simulate PVC becoming Bound
			pvc.Status.Phase = corev1.ClaimBound
			pvc.Status.Capacity = corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("5Gi"),
			}
			Expect(k8sClient.Status().Update(ctx, &pvc)).To(Succeed())

			// Reconcile again: transitions to Ready
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, wsKey, &updatedWS)).To(Succeed())
			Expect(updatedWS.Status.Phase).To(Equal(sandboxesv1alpha1.WorkspacePhaseReady))
			Expect(updatedWS.Status.Capacity).To(Equal("5Gi"))
		})

		It("should sweep stale BoundSandboxRef when referenced sandbox does not exist", func() {
			wsName := "test-workspace-stale-sweep"
			wsKey := types.NamespacedName{Name: wsName, Namespace: namespace}

			workspace := &sandboxesv1alpha1.Workspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:      wsName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.WorkspaceSpec{
					Size: "2Gi",
				},
			}
			Expect(k8sClient.Create(ctx, workspace)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, workspace) }()

			// Set status as Bound to non-existent sandbox
			workspace.Status.Phase = sandboxesv1alpha1.WorkspacePhaseBound
			workspace.Status.BoundSandboxRef = &corev1.LocalObjectReference{Name: "ghost-sandbox"}
			Expect(k8sClient.Status().Update(ctx, workspace)).To(Succeed())

			reconciler := &WorkspaceReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// Reconcile: first adds finalizer, second sweeps
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
			Expect(err).NotTo(HaveOccurred())

			var updatedWS sandboxesv1alpha1.Workspace
			Expect(k8sClient.Get(ctx, wsKey, &updatedWS)).To(Succeed())
			Expect(updatedWS.Status.BoundSandboxRef).To(BeNil())
			Expect(updatedWS.Status.Phase).To(Equal(sandboxesv1alpha1.WorkspacePhasePending))
		})

		It("should delete underlying PVC when ReclaimPolicy is Delete", func() {
			wsName := "test-workspace-delete-policy"
			wsKey := types.NamespacedName{Name: wsName, Namespace: namespace}
			pvcKey := types.NamespacedName{Name: "ws-" + wsName + "-pvc", Namespace: namespace}

			workspace := &sandboxesv1alpha1.Workspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:      wsName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.WorkspaceSpec{
					Size:          "1Gi",
					ReclaimPolicy: sandboxesv1alpha1.WorkspaceReclaimDelete,
				},
			}
			Expect(k8sClient.Create(ctx, workspace)).To(Succeed())

			reconciler := &WorkspaceReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// Create PVC: first adds finalizer, second creates PVC
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
			Expect(err).NotTo(HaveOccurred())

			var pvc corev1.PersistentVolumeClaim
			Expect(k8sClient.Get(ctx, pvcKey, &pvc)).To(Succeed())

			// Delete Workspace
			Expect(k8sClient.Delete(ctx, workspace)).To(Succeed())

			// Reconcile deletion
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
			Expect(err).NotTo(HaveOccurred())

			// Verify PVC was deleted or marked for deletion
			err = k8sClient.Get(ctx, pvcKey, &pvc)
			Expect(errors.IsNotFound(err) || !pvc.DeletionTimestamp.IsZero()).To(BeTrue())
		})
		It("should map unowned and retained PVC changes to referencing Workspaces via findWorkspacesForPVC", func() {
			wsName := "test-map-ws"
			wsKey := types.NamespacedName{Name: wsName, Namespace: namespace}

			workspace := &sandboxesv1alpha1.Workspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:      wsName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.WorkspaceSpec{
					Size: "2Gi",
				},
			}
			Expect(k8sClient.Create(ctx, workspace)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, workspace) }()

			reconciler := &WorkspaceReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			pvc := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pvc-for-" + wsName,
					Namespace: namespace,
				},
			}
			reqs := reconciler.findWorkspacesForPVC(ctx, pvc)
			Expect(reqs).To(HaveLen(1))
			Expect(reqs[0].NamespacedName).To(Equal(wsKey))
		})
		It("should retain PVC and strip ownerReference when reclaim policy is Retain on delete", func() {
			wsName := "test-retain-owner-ws"
			wsKey := types.NamespacedName{Name: wsName, Namespace: namespace}
			pvcKey := types.NamespacedName{Name: "ws-" + wsName + "-pvc", Namespace: namespace}

			workspace := &sandboxesv1alpha1.Workspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:      wsName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.WorkspaceSpec{
					Size:          "1Gi",
					ReclaimPolicy: sandboxesv1alpha1.WorkspaceReclaimRetain,
				},
			}
			Expect(k8sClient.Create(ctx, workspace)).To(Succeed())

			reconciler := &WorkspaceReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// Add finalizer
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
			Expect(err).NotTo(HaveOccurred())
			// Create PVC
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
			Expect(err).NotTo(HaveOccurred())

			var pvc corev1.PersistentVolumeClaim
			Expect(k8sClient.Get(ctx, pvcKey, &pvc)).To(Succeed())
			Expect(hasWorkspaceOwnerReference(&pvc, workspace)).To(BeFalse())

			// Delete Workspace
			Expect(k8sClient.Delete(ctx, workspace)).To(Succeed())

			// Reconcile deletion
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
			Expect(err).NotTo(HaveOccurred())

			// PVC must still exist and have no owner reference to workspace
			Expect(k8sClient.Get(ctx, pvcKey, &pvc)).To(Succeed())
			Expect(hasWorkspaceOwnerReference(&pvc, workspace)).To(BeFalse())

			// Clean up PVC
			_ = k8sClient.Delete(ctx, &pvc)
		})
		It("should fail workspace reconciliation if git subpath is invalid", func() {
			wsName := "test-invalid-git-ws"
			wsKey := types.NamespacedName{Name: wsName, Namespace: namespace}

			workspace := &sandboxesv1alpha1.Workspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:      wsName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.WorkspaceSpec{
					Size: "1Gi",
					Git: &sandboxesv1alpha1.GitRepositorySpec{
						URL:     "https://github.com/foo/bar.git",
						Subpath: "../escape",
					},
				},
			}
			Expect(k8sClient.Create(ctx, workspace)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, workspace) }()

			reconciler := &WorkspaceReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
			Expect(err).NotTo(HaveOccurred())

			var updatedWS sandboxesv1alpha1.Workspace
			Expect(k8sClient.Get(ctx, wsKey, &updatedWS)).To(Succeed())
			Expect(updatedWS.Status.Phase).To(Equal(sandboxesv1alpha1.WorkspacePhaseFailed))
		})

		It("should prevent workspace deletion while bound sandbox is still active", func() {
			wsName := "test-active-del-ws"
			wsKey := types.NamespacedName{Name: wsName, Namespace: namespace}
			sbxName := "test-active-del-sbx"
			podName := sbxName + "-sandbox"

			workspace := &sandboxesv1alpha1.Workspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:      wsName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.WorkspaceSpec{
					Size:          "1Gi",
					ReclaimPolicy: sandboxesv1alpha1.WorkspaceReclaimRetain,
				},
			}
			Expect(k8sClient.Create(ctx, workspace)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, workspace) }()

			reconciler := &WorkspaceReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
			Expect(err).NotTo(HaveOccurred())

			// Simulate bound active Sandbox and Pod
			Expect(k8sClient.Get(ctx, wsKey, workspace)).To(Succeed())
			workspace.Status.BoundSandboxRef = &corev1.LocalObjectReference{Name: sbxName}
			workspace.Status.Phase = sandboxesv1alpha1.WorkspacePhaseBound
			Expect(k8sClient.Status().Update(ctx, workspace)).To(Succeed())

			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: sbxName, Namespace: namespace},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image:   "alpine:latest",
					Command: []string{"sh", "-c"},
					Args:    []string{"echo"},
				},
			}
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, sandbox) }()

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "test", Image: "alpine"}},
				},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, pod) }()

			// Delete workspace
			Expect(k8sClient.Delete(ctx, workspace)).To(Succeed())

			// Reconcile deletion: should requeue because bound pod is active
			res, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))

			var checkWS sandboxesv1alpha1.Workspace
			Expect(k8sClient.Get(ctx, wsKey, &checkWS)).To(Succeed())
			Expect(checkWS.Finalizers).To(ContainElement(workspaceRetentionFinalizer))
		})

		It("should fail workspace reconciliation if unmanaged PVC collides with dynamic PVC name", func() {
			wsName := "test-collision-ws"
			wsKey := types.NamespacedName{Name: wsName, Namespace: namespace}
			pvcKey := types.NamespacedName{Name: "ws-" + wsName + "-pvc", Namespace: namespace}

			pvc := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      pvcKey.Name,
					Namespace: namespace,
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("1Gi"),
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, pvc) }()

			workspace := &sandboxesv1alpha1.Workspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:      wsName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.WorkspaceSpec{
					Size: "1Gi",
				},
			}
			Expect(k8sClient.Create(ctx, workspace)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, workspace) }()

			reconciler := &WorkspaceReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
			Expect(err).NotTo(HaveOccurred())

			var updatedWS sandboxesv1alpha1.Workspace
			Expect(k8sClient.Get(ctx, wsKey, &updatedWS)).To(Succeed())
			Expect(updatedWS.Status.Phase).To(Equal(sandboxesv1alpha1.WorkspacePhaseFailed))
			cond := meta.FindStatusCondition(updatedWS.Status.Conditions, sandboxesv1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal("StorageCollision"))
		})

		It("should allow Workspace to adopt explicit external PVC reference without collision error", func() {
			customPVCName := "custom-external-pvc"
			pvc := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      customPVCName,
					Namespace: namespace,
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("1Gi"),
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, pvc) }()

			pvc.Status.Phase = corev1.ClaimBound
			pvc.Status.Capacity = corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("1Gi"),
			}
			Expect(k8sClient.Status().Update(ctx, pvc)).To(Succeed())

			wsName := "test-explicit-adopt-ws"
			wsKey := types.NamespacedName{Name: wsName, Namespace: namespace}
			workspace := &sandboxesv1alpha1.Workspace{
				ObjectMeta: metav1.ObjectMeta{
					Name:      wsName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.WorkspaceSpec{
					ReclaimPolicy: sandboxesv1alpha1.WorkspaceReclaimDelete,
					PersistentVolumeClaim: &sandboxesv1alpha1.PersistentVolumeClaimWorkspace{
						ClaimName: customPVCName,
					},
				},
			}
			Expect(k8sClient.Create(ctx, workspace)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, workspace) }()

			reconciler := &WorkspaceReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, _ = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
			Expect(err).NotTo(HaveOccurred())

			var updatedWS sandboxesv1alpha1.Workspace
			Expect(k8sClient.Get(ctx, wsKey, &updatedWS)).To(Succeed())
			Expect(updatedWS.Status.Phase).To(Equal(sandboxesv1alpha1.WorkspacePhaseReady))
			Expect(updatedWS.Status.PVCName).To(Equal(customPVCName))

			// External PVC must not have controller owner reference
			var checkPVC corev1.PersistentVolumeClaim
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: customPVCName, Namespace: namespace}, &checkPVC)).To(Succeed())
			Expect(hasWorkspaceOwnerReference(&checkPVC, &updatedWS)).To(BeFalse())

			// Deleting workspace must not delete external PVC
			Expect(k8sClient.Delete(ctx, workspace)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: customPVCName, Namespace: namespace}, &checkPVC)).To(Succeed())
		})
	})
})
