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
	})
})
