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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
)

type errorInjectingClient struct {
	client.Client
	deleteErrTarget     string
	deleteErr           error
	getErrTarget        string
	getErr              error
	patchConflictTarget string
}

func (c *errorInjectingClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if c.deleteErr != nil && (c.deleteErrTarget == "" || obj.GetName() == c.deleteErrTarget) {
		return c.deleteErr
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func (c *errorInjectingClient) Get(ctx context.Context, key types.NamespacedName, obj client.Object, opts ...client.GetOption) error {
	if c.getErr != nil && (c.getErrTarget == "" || key.Name == c.getErrTarget) {
		return c.getErr
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *errorInjectingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if c.patchConflictTarget != "" && obj.GetName() == c.patchConflictTarget {
		return errors.NewConflict(sandboxesv1alpha1.GroupVersion.WithResource("sandboxes").GroupResource(), obj.GetName(), fmt.Errorf("simulated conflict"))
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

var _ = Describe("SandboxLease Controller", func() {
	const namespace = "default"

	Context("When reconciling a SandboxLease", func() {
		It("should add the lease-protection finalizer on creation", func() {
			leaseName := "test-lease-finalizer"
			leaseKey := types.NamespacedName{Name: leaseName, Namespace: namespace}

			lease := &sandboxesv1alpha1.SandboxLease{
				ObjectMeta: metav1.ObjectMeta{
					Name:      leaseName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.SandboxLeaseSpec{
					PoolRef:        corev1.LocalObjectReference{Name: "test-pool"},
					ExecutionId:    "exec-fin-1",
					TimeoutSeconds: 300,
				},
			}
			Expect(k8sClient.Create(ctx, lease)).To(Succeed())
			defer func() {
				_ = k8sClient.Get(ctx, leaseKey, lease)
				controllerutil.RemoveFinalizer(lease, sandboxesv1alpha1.LeaseProtectionFinalizer)
				_ = k8sClient.Update(ctx, lease)
				_ = k8sClient.Delete(ctx, lease)
			}()

			reconciler := &SandboxLeaseReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(10),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: leaseKey})
			Expect(err).NotTo(HaveOccurred())

			var fetched sandboxesv1alpha1.SandboxLease
			Expect(k8sClient.Get(ctx, leaseKey, &fetched)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(&fetched, sandboxesv1alpha1.LeaseProtectionFinalizer)).To(BeTrue())
		})

		It("should bind an available standby runner via atomic CAS claim", func() {
			poolName := "test-pool-claim"
			sbxName := "test-standby-runner-1"
			leaseName := "test-lease-claim-1"
			leaseKey := types.NamespacedName{Name: leaseName, Namespace: namespace}

			// Create a ready standby sandbox
			boolTrue := true
			sbx := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sbxName,
					Namespace: namespace,
					Labels: map[string]string{
						sandboxesv1alpha1.LabelPoolName:   poolName,
						sandboxesv1alpha1.LabelPoolStatus: sandboxesv1alpha1.PoolStatusStandby,
					},
				},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image:   "ghcr.io/hoangndst/buddy-sandboxes-base:latest",
					Command: []string{"sleep", "infinity"},
					Workspace: sandboxesv1alpha1.WorkspaceSpec{
						MountPath: "/workspace",
						Ephemeral: &boolTrue,
					},
				},
			}
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, sbx) }()

			// Mark sandbox as Running with PodName in status
			sbx.Status.Phase = sandboxesv1alpha1.SandboxPhaseRunning
			sbx.Status.PodName = sbxName + "-pod"
			Expect(k8sClient.Status().Update(ctx, sbx)).To(Succeed())

			// Create lease
			lease := &sandboxesv1alpha1.SandboxLease{
				ObjectMeta: metav1.ObjectMeta{
					Name:       leaseName,
					Namespace:  namespace,
					Finalizers: []string{sandboxesv1alpha1.LeaseProtectionFinalizer},
				},
				Spec: sandboxesv1alpha1.SandboxLeaseSpec{
					PoolRef:        corev1.LocalObjectReference{Name: poolName},
					ExecutionId:    "exec-claim-1",
					TimeoutSeconds: 120,
				},
			}
			Expect(k8sClient.Create(ctx, lease)).To(Succeed())
			defer func() {
				_ = k8sClient.Get(ctx, leaseKey, lease)
				controllerutil.RemoveFinalizer(lease, sandboxesv1alpha1.LeaseProtectionFinalizer)
				_ = k8sClient.Update(ctx, lease)
				_ = k8sClient.Delete(ctx, lease)
			}()

			reconciler := &SandboxLeaseReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(10),
			}

			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: leaseKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			// Check that sandbox labels and annotations were updated via CAS
			var updatedSbx sandboxesv1alpha1.Sandbox
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbxName, Namespace: namespace}, &updatedSbx)).To(Succeed())
			Expect(updatedSbx.Labels[sandboxesv1alpha1.LabelPoolStatus]).To(Equal(sandboxesv1alpha1.PoolStatusBound))
			Expect(updatedSbx.Labels[sandboxesv1alpha1.LabelLeaseName]).To(Equal(leaseName))
			Expect(updatedSbx.Annotations[sandboxesv1alpha1.AnnotationClaimedAt]).NotTo(BeEmpty())
			Expect(updatedSbx.Annotations[sandboxesv1alpha1.AnnotationClaimDeadline]).To(BeEmpty())

			// Check that lease status transitioned to Bound
			var updatedLease sandboxesv1alpha1.SandboxLease
			Expect(k8sClient.Get(ctx, leaseKey, &updatedLease)).To(Succeed())
			Expect(updatedLease.Status.Phase).To(Equal(sandboxesv1alpha1.SandboxLeasePhaseBound))
			Expect(updatedLease.Status.BoundSandboxRef).NotTo(BeNil())
			Expect(updatedLease.Status.BoundSandboxRef.Name).To(Equal(sbxName))
			Expect(updatedLease.Status.BoundPodName).To(Equal(sbxName + "-pod"))
			Expect(updatedLease.Status.LeasedAt).NotTo(BeNil())
			Expect(updatedLease.Status.ExpiresAt).NotTo(BeNil())
		})

		It("should cleanly delete claimed sandbox upon explicit lease release", func() {
			poolName := "test-pool-release"
			sbxName := "test-standby-runner-release"
			leaseName := "test-lease-release-1"
			leaseKey := types.NamespacedName{Name: leaseName, Namespace: namespace}

			boolTrue := true
			sbx := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sbxName,
					Namespace: namespace,
					Labels: map[string]string{
						sandboxesv1alpha1.LabelPoolName:   poolName,
						sandboxesv1alpha1.LabelPoolStatus: sandboxesv1alpha1.PoolStatusClaimed,
						sandboxesv1alpha1.LabelLeaseName:  leaseName,
					},
				},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image:   "ghcr.io/hoangndst/buddy-sandboxes-base:latest",
					Command: []string{"sleep", "infinity"},
					Workspace: sandboxesv1alpha1.WorkspaceSpec{
						MountPath: "/workspace",
						Ephemeral: &boolTrue,
					},
				},
			}
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, sbx) }()

			lease := &sandboxesv1alpha1.SandboxLease{
				ObjectMeta: metav1.ObjectMeta{
					Name:       leaseName,
					Namespace:  namespace,
					Finalizers: []string{sandboxesv1alpha1.LeaseProtectionFinalizer},
				},
				Spec: sandboxesv1alpha1.SandboxLeaseSpec{
					PoolRef:        corev1.LocalObjectReference{Name: poolName},
					ExecutionId:    "exec-rel-1",
					TimeoutSeconds: 120,
					Released:       true, // Explicit release
				},
			}
			Expect(k8sClient.Create(ctx, lease)).To(Succeed())
			defer func() {
				_ = k8sClient.Get(ctx, leaseKey, lease)
				controllerutil.RemoveFinalizer(lease, sandboxesv1alpha1.LeaseProtectionFinalizer)
				_ = k8sClient.Update(ctx, lease)
				_ = k8sClient.Delete(ctx, lease)
			}()

			// Set bound status
			lease.Status.Phase = sandboxesv1alpha1.SandboxLeasePhaseBound
			lease.Status.BoundSandboxRef = &corev1.LocalObjectReference{Name: sbxName}
			Expect(k8sClient.Status().Update(ctx, lease)).To(Succeed())

			reconciler := &SandboxLeaseReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(10),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: leaseKey})
			Expect(err).NotTo(HaveOccurred())

			// Child sandbox should be deleted
			var fetchedSbx sandboxesv1alpha1.Sandbox
			err = k8sClient.Get(ctx, types.NamespacedName{Name: sbxName, Namespace: namespace}, &fetchedSbx)
			Expect(errors.IsNotFound(err)).To(BeTrue())

			// Lease phase should be Released
			var updatedLease sandboxesv1alpha1.SandboxLease
			Expect(k8sClient.Get(ctx, leaseKey, &updatedLease)).To(Succeed())
			Expect(updatedLease.Status.Phase).To(Equal(sandboxesv1alpha1.SandboxLeasePhaseReleased))
		})

		It("should expire lease and delete runner when timeout is reached", func() {
			poolName := "test-pool-expire"
			sbxName := "test-standby-runner-expire"
			leaseName := "test-lease-expire-1"
			leaseKey := types.NamespacedName{Name: leaseName, Namespace: namespace}

			boolTrue := true
			sbx := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sbxName,
					Namespace: namespace,
					Labels: map[string]string{
						sandboxesv1alpha1.LabelPoolName:   poolName,
						sandboxesv1alpha1.LabelPoolStatus: sandboxesv1alpha1.PoolStatusClaimed,
					},
				},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image:   "ghcr.io/hoangndst/buddy-sandboxes-base:latest",
					Command: []string{"sleep", "infinity"},
					Workspace: sandboxesv1alpha1.WorkspaceSpec{
						MountPath: "/workspace",
						Ephemeral: &boolTrue,
					},
				},
			}
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, sbx) }()

			lease := &sandboxesv1alpha1.SandboxLease{
				ObjectMeta: metav1.ObjectMeta{
					Name:       leaseName,
					Namespace:  namespace,
					Finalizers: []string{sandboxesv1alpha1.LeaseProtectionFinalizer},
				},
				Spec: sandboxesv1alpha1.SandboxLeaseSpec{
					PoolRef:        corev1.LocalObjectReference{Name: poolName},
					ExecutionId:    "exec-exp-1",
					TimeoutSeconds: 10,
				},
			}
			Expect(k8sClient.Create(ctx, lease)).To(Succeed())
			defer func() {
				_ = k8sClient.Get(ctx, leaseKey, lease)
				controllerutil.RemoveFinalizer(lease, sandboxesv1alpha1.LeaseProtectionFinalizer)
				_ = k8sClient.Update(ctx, lease)
				_ = k8sClient.Delete(ctx, lease)
			}()

			// Set already-expired timestamp
			pastTime := metav1.NewTime(time.Now().Add(-10 * time.Second))
			lease.Status.Phase = sandboxesv1alpha1.SandboxLeasePhaseBound
			lease.Status.BoundSandboxRef = &corev1.LocalObjectReference{Name: sbxName}
			lease.Status.ExpiresAt = &pastTime
			Expect(k8sClient.Status().Update(ctx, lease)).To(Succeed())

			reconciler := &SandboxLeaseReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(10),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: leaseKey})
			Expect(err).NotTo(HaveOccurred())

			// Child sandbox should be deleted
			var fetchedSbx sandboxesv1alpha1.Sandbox
			err = k8sClient.Get(ctx, types.NamespacedName{Name: sbxName, Namespace: namespace}, &fetchedSbx)
			Expect(errors.IsNotFound(err)).To(BeTrue())

			// Lease phase should be Expired
			var updatedLease sandboxesv1alpha1.SandboxLease
			Expect(k8sClient.Get(ctx, leaseKey, &updatedLease)).To(Succeed())
			Expect(updatedLease.Status.Phase).To(Equal(sandboxesv1alpha1.SandboxLeasePhaseExpired))
		})

		It("should enforce CEL validation rules for immutability on SandboxLease", func() {
			leaseName := "test-lease-cel"
			lease := &sandboxesv1alpha1.SandboxLease{
				ObjectMeta: metav1.ObjectMeta{
					Name:      leaseName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.SandboxLeaseSpec{
					PoolRef:        corev1.LocalObjectReference{Name: "pool-a"},
					ExecutionId:    "exec-cel-1",
					TimeoutSeconds: 100,
				},
			}
			Expect(k8sClient.Create(ctx, lease)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, lease) }()

			// Attempt to mutate immutable poolRef
			lease.Spec.PoolRef.Name = "pool-b"
			err := k8sClient.Update(ctx, lease)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("poolRef is immutable after creation"))
		})

		It("should detect concurrent claim conflict via optimistic locking and fall back to next candidate", func() {
			poolName := "test-pool-optimistic-conflict"
			sbx1Name := "test-runner-conflict-1"
			sbx2Name := "test-runner-conflict-2"
			leaseName := "test-lease-optimistic-1"
			leaseKey := types.NamespacedName{Name: leaseName, Namespace: namespace}

			boolTrue := true
			createStandby := func(name string) *sandboxesv1alpha1.Sandbox {
				sbx := &sandboxesv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: namespace,
						Labels: map[string]string{
							sandboxesv1alpha1.LabelPoolName:   poolName,
							sandboxesv1alpha1.LabelPoolStatus: sandboxesv1alpha1.PoolStatusStandby,
						},
					},
					Spec: sandboxesv1alpha1.SandboxSpec{
						Image:   "ghcr.io/hoangndst/buddy-sandboxes-base:latest",
						Command: []string{"sleep", "infinity"},
						Workspace: sandboxesv1alpha1.WorkspaceSpec{
							MountPath: "/workspace",
							Ephemeral: &boolTrue,
						},
					},
				}
				Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
				sbx.Status.Phase = sandboxesv1alpha1.SandboxPhaseRunning
				sbx.Status.PodName = name + "-pod"
				Expect(k8sClient.Status().Update(ctx, sbx)).To(Succeed())
				return sbx
			}

			sbx1 := createStandby(sbx1Name)
			defer func() { _ = k8sClient.Delete(ctx, sbx1) }()
			sbx2 := createStandby(sbx2Name)
			defer func() { _ = k8sClient.Delete(ctx, sbx2) }()

			lease := &sandboxesv1alpha1.SandboxLease{
				ObjectMeta: metav1.ObjectMeta{
					Name:       leaseName,
					Namespace:  namespace,
					Finalizers: []string{sandboxesv1alpha1.LeaseProtectionFinalizer},
				},
				Spec: sandboxesv1alpha1.SandboxLeaseSpec{
					PoolRef:        corev1.LocalObjectReference{Name: poolName},
					ExecutionId:    "exec-opt-1",
					TimeoutSeconds: 60,
				},
			}
			Expect(k8sClient.Create(ctx, lease)).To(Succeed())
			defer func() {
				_ = k8sClient.Get(ctx, leaseKey, lease)
				controllerutil.RemoveFinalizer(lease, sandboxesv1alpha1.LeaseProtectionFinalizer)
				_ = k8sClient.Update(ctx, lease)
				_ = k8sClient.Delete(ctx, lease)
			}()

			injectingClient := &errorInjectingClient{
				Client:              k8sClient,
				patchConflictTarget: sbx1Name,
			}
			reconciler := &SandboxLeaseReconciler{
				Client:   injectingClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(10),
			}

			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: leaseKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))

			var updatedLease sandboxesv1alpha1.SandboxLease
			Expect(k8sClient.Get(ctx, leaseKey, &updatedLease)).To(Succeed())
			Expect(updatedLease.Status.Phase).To(Equal(sandboxesv1alpha1.SandboxLeasePhaseBound))
			Expect(updatedLease.Status.BoundSandboxRef).NotTo(BeNil())
			Expect(updatedLease.Status.BoundSandboxRef.Name).To(Equal(sbx2Name))
		})

		It("should retain finalizer and propagate error when runner cleanup fails during lease deletion", func() {
			poolName := "test-pool-del-retry"
			sbxName := "test-runner-del-retry"
			leaseName := "test-lease-del-retry"
			leaseKey := types.NamespacedName{Name: leaseName, Namespace: namespace}

			boolTrue := true
			sbx := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sbxName,
					Namespace: namespace,
					Labels: map[string]string{
						sandboxesv1alpha1.LabelPoolName:   poolName,
						sandboxesv1alpha1.LabelPoolStatus: sandboxesv1alpha1.PoolStatusClaimed,
					},
				},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image:   "ghcr.io/hoangndst/buddy-sandboxes-base:latest",
					Command: []string{"sleep", "infinity"},
					Workspace: sandboxesv1alpha1.WorkspaceSpec{
						MountPath: "/workspace",
						Ephemeral: &boolTrue,
					},
				},
			}
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, sbx) }()

			lease := &sandboxesv1alpha1.SandboxLease{
				ObjectMeta: metav1.ObjectMeta{
					Name:       leaseName,
					Namespace:  namespace,
					Finalizers: []string{sandboxesv1alpha1.LeaseProtectionFinalizer},
				},
				Spec: sandboxesv1alpha1.SandboxLeaseSpec{
					PoolRef:        corev1.LocalObjectReference{Name: poolName},
					ExecutionId:    "exec-del-retry",
					TimeoutSeconds: 60,
				},
			}
			Expect(k8sClient.Create(ctx, lease)).To(Succeed())
			defer func() {
				_ = k8sClient.Get(ctx, leaseKey, lease)
				controllerutil.RemoveFinalizer(lease, sandboxesv1alpha1.LeaseProtectionFinalizer)
				_ = k8sClient.Update(ctx, lease)
				_ = k8sClient.Delete(ctx, lease)
			}()

			lease.Status.Phase = sandboxesv1alpha1.SandboxLeasePhaseBound
			lease.Status.BoundSandboxRef = &corev1.LocalObjectReference{Name: sbxName}
			Expect(k8sClient.Status().Update(ctx, lease)).To(Succeed())

			Expect(k8sClient.Delete(ctx, lease)).To(Succeed())

			injectingClient := &errorInjectingClient{
				Client:          k8sClient,
				deleteErrTarget: sbxName,
				deleteErr:       errors.NewInternalError(fmt.Errorf("simulated etcd write timeout")),
			}
			reconciler := &SandboxLeaseReconciler{
				Client:   injectingClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(10),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: leaseKey})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("simulated etcd write timeout"))

			var fetchedLease sandboxesv1alpha1.SandboxLease
			Expect(k8sClient.Get(ctx, leaseKey, &fetchedLease)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(&fetchedLease, sandboxesv1alpha1.LeaseProtectionFinalizer)).To(BeTrue())

			healthyReconciler := &SandboxLeaseReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(10),
			}
			_, err = healthyReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: leaseKey})
			Expect(err).NotTo(HaveOccurred())

			err = k8sClient.Get(ctx, leaseKey, &fetchedLease)
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		It("should propagate error and retain Bound phase when runner cleanup fails during explicit release", func() {
			poolName := "test-pool-rel-retry"
			sbxName := "test-runner-rel-retry"
			leaseName := "test-lease-rel-retry"
			leaseKey := types.NamespacedName{Name: leaseName, Namespace: namespace}

			boolTrue := true
			sbx := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sbxName,
					Namespace: namespace,
					Labels: map[string]string{
						sandboxesv1alpha1.LabelPoolName:   poolName,
						sandboxesv1alpha1.LabelPoolStatus: sandboxesv1alpha1.PoolStatusClaimed,
					},
				},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image:   "ghcr.io/hoangndst/buddy-sandboxes-base:latest",
					Command: []string{"sleep", "infinity"},
					Workspace: sandboxesv1alpha1.WorkspaceSpec{
						MountPath: "/workspace",
						Ephemeral: &boolTrue,
					},
				},
			}
			Expect(k8sClient.Create(ctx, sbx)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, sbx) }()

			lease := &sandboxesv1alpha1.SandboxLease{
				ObjectMeta: metav1.ObjectMeta{
					Name:       leaseName,
					Namespace:  namespace,
					Finalizers: []string{sandboxesv1alpha1.LeaseProtectionFinalizer},
				},
				Spec: sandboxesv1alpha1.SandboxLeaseSpec{
					PoolRef:        corev1.LocalObjectReference{Name: poolName},
					ExecutionId:    "exec-rel-retry",
					TimeoutSeconds: 60,
					Released:       true,
				},
			}
			Expect(k8sClient.Create(ctx, lease)).To(Succeed())
			defer func() {
				_ = k8sClient.Get(ctx, leaseKey, lease)
				controllerutil.RemoveFinalizer(lease, sandboxesv1alpha1.LeaseProtectionFinalizer)
				_ = k8sClient.Update(ctx, lease)
				_ = k8sClient.Delete(ctx, lease)
			}()

			lease.Status.Phase = sandboxesv1alpha1.SandboxLeasePhaseBound
			lease.Status.BoundSandboxRef = &corev1.LocalObjectReference{Name: sbxName}
			Expect(k8sClient.Status().Update(ctx, lease)).To(Succeed())

			injectingClient := &errorInjectingClient{
				Client:          k8sClient,
				deleteErrTarget: sbxName,
				deleteErr:       errors.NewInternalError(fmt.Errorf("simulated transient delete error")),
			}
			reconciler := &SandboxLeaseReconciler{
				Client:   injectingClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(10),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: leaseKey})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("simulated transient delete error"))

			var fetchedLease sandboxesv1alpha1.SandboxLease
			Expect(k8sClient.Get(ctx, leaseKey, &fetchedLease)).To(Succeed())
			Expect(fetchedLease.Status.Phase).To(Equal(sandboxesv1alpha1.SandboxLeasePhaseBound))

			healthyReconciler := &SandboxLeaseReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(10),
			}
			_, err = healthyReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: leaseKey})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, leaseKey, &fetchedLease)).To(Succeed())
			Expect(fetchedLease.Status.Phase).To(Equal(sandboxesv1alpha1.SandboxLeasePhaseReleased))
		})
	})
})
