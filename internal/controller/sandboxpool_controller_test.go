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

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
)

var _ = Describe("SandboxPool Controller", func() {
	const namespace = "default"

	Context("When reconciling a SandboxPool", func() {
		It("should provision standby sandboxes to satisfy minWarm", func() {
			poolName := "test-pool-provision"
			poolKey := types.NamespacedName{Name: poolName, Namespace: namespace}

			pool := &sandboxesv1alpha1.SandboxPool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      poolName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.SandboxPoolSpec{
					MinWarm: 2,
					MaxWarm: 5,
					Template: sandboxesv1alpha1.SandboxTemplateSpec{
						Image: "ghcr.io/hoangndst/buddy-sandboxes-base:latest",
					},
				},
			}
			Expect(k8sClient.Create(ctx, pool)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, pool)
				var list sandboxesv1alpha1.SandboxList
				_ = k8sClient.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabels{sandboxesv1alpha1.LabelPoolName: poolName})
				for _, item := range list.Items {
					_ = k8sClient.Delete(ctx, &item)
				}
			}()

			reconciler := &SandboxPoolReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(10),
			}

			// First reconcile creates the standby sandboxes
			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: poolKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())

			// Verify child sandboxes were created
			var sbxList sandboxesv1alpha1.SandboxList
			Expect(k8sClient.List(ctx, &sbxList, client.InNamespace(namespace), client.MatchingLabels{sandboxesv1alpha1.LabelPoolName: poolName})).To(Succeed())
			Expect(sbxList.Items).To(HaveLen(2))

			for _, sbx := range sbxList.Items {
				Expect(sbx.Labels[sandboxesv1alpha1.LabelPoolStatus]).To(Equal(sandboxesv1alpha1.PoolStatusStandby))
				Expect(sbx.Spec.Command).To(Equal([]string{"sleep", "infinity"}))
				Expect(sbx.Spec.Workspace.Ephemeral).NotTo(BeNil())
				Expect(*sbx.Spec.Workspace.Ephemeral).To(BeTrue())
			}
		})

		It("should immediately replenish standby capacity when a runner is claimed", func() {
			poolName := "test-pool-replenish"
			poolKey := types.NamespacedName{Name: poolName, Namespace: namespace}

			pool := &sandboxesv1alpha1.SandboxPool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      poolName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.SandboxPoolSpec{
					MinWarm: 2,
					MaxWarm: 4,
					Template: sandboxesv1alpha1.SandboxTemplateSpec{
						Image: "ghcr.io/hoangndst/buddy-sandboxes-base:latest",
					},
				},
			}
			Expect(k8sClient.Create(ctx, pool)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, pool)
				var list sandboxesv1alpha1.SandboxList
				_ = k8sClient.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabels{sandboxesv1alpha1.LabelPoolName: poolName})
				for _, item := range list.Items {
					_ = k8sClient.Delete(ctx, &item)
				}
			}()

			reconciler := &SandboxPoolReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(10),
			}

			// Initial provisioning
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: poolKey})
			Expect(err).NotTo(HaveOccurred())

			var sbxList sandboxesv1alpha1.SandboxList
			Expect(k8sClient.List(ctx, &sbxList, client.InNamespace(namespace), client.MatchingLabels{sandboxesv1alpha1.LabelPoolName: poolName})).To(Succeed())
			Expect(sbxList.Items).To(HaveLen(2))

			// Simulate runner claim on the first sandbox
			claimedSbx := &sbxList.Items[0]
			claimedSbx.Labels[sandboxesv1alpha1.LabelPoolStatus] = sandboxesv1alpha1.PoolStatusClaimed
			Expect(k8sClient.Update(ctx, claimedSbx)).To(Succeed())

			// Reconcile should detect standby deficit and spawn 1 replacement
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: poolKey})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.List(ctx, &sbxList, client.InNamespace(namespace), client.MatchingLabels{sandboxesv1alpha1.LabelPoolName: poolName})).To(Succeed())
			Expect(sbxList.Items).To(HaveLen(3)) // 1 claimed + 2 standby
		})

		It("should strictly respect maxWarm ceiling and not over-provision", func() {
			poolName := "test-pool-maxwarm"
			poolKey := types.NamespacedName{Name: poolName, Namespace: namespace}

			pool := &sandboxesv1alpha1.SandboxPool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      poolName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.SandboxPoolSpec{
					MinWarm: 3,
					MaxWarm: 3, // Capacity capped strictly at 3
					Template: sandboxesv1alpha1.SandboxTemplateSpec{
						Image: "ghcr.io/hoangndst/buddy-sandboxes-base:latest",
					},
				},
			}
			Expect(k8sClient.Create(ctx, pool)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, pool)
				var list sandboxesv1alpha1.SandboxList
				_ = k8sClient.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabels{sandboxesv1alpha1.LabelPoolName: poolName})
				for _, item := range list.Items {
					_ = k8sClient.Delete(ctx, &item)
				}
			}()

			reconciler := &SandboxPoolReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(10),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: poolKey})
			Expect(err).NotTo(HaveOccurred())

			var sbxList sandboxesv1alpha1.SandboxList
			Expect(k8sClient.List(ctx, &sbxList, client.InNamespace(namespace), client.MatchingLabels{sandboxesv1alpha1.LabelPoolName: poolName})).To(Succeed())
			Expect(sbxList.Items).To(HaveLen(3))

			// Reconciling again should not create additional runners
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: poolKey})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.List(ctx, &sbxList, client.InNamespace(namespace), client.MatchingLabels{sandboxesv1alpha1.LabelPoolName: poolName})).To(Succeed())
			Expect(sbxList.Items).To(HaveLen(3))
		})

		It("should clean up terminal runners", func() {
			poolName := "test-pool-cleanup-terminal"
			poolKey := types.NamespacedName{Name: poolName, Namespace: namespace}

			pool := &sandboxesv1alpha1.SandboxPool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      poolName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.SandboxPoolSpec{
					MinWarm: 1,
					MaxWarm: 2,
					Template: sandboxesv1alpha1.SandboxTemplateSpec{
						Image: "ghcr.io/hoangndst/buddy-sandboxes-base:latest",
					},
				},
			}
			Expect(k8sClient.Create(ctx, pool)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, pool)
				var list sandboxesv1alpha1.SandboxList
				_ = k8sClient.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabels{sandboxesv1alpha1.LabelPoolName: poolName})
				for _, item := range list.Items {
					_ = k8sClient.Delete(ctx, &item)
				}
			}()

			reconciler := &SandboxPoolReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(10),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: poolKey})
			Expect(err).NotTo(HaveOccurred())

			var sbxList sandboxesv1alpha1.SandboxList
			Expect(k8sClient.List(ctx, &sbxList, client.InNamespace(namespace), client.MatchingLabels{sandboxesv1alpha1.LabelPoolName: poolName})).To(Succeed())
			Expect(sbxList.Items).To(HaveLen(1))

			// Mark sandbox terminal
			sbx := &sbxList.Items[0]
			sbx.Status.Phase = sandboxesv1alpha1.SandboxPhaseFailed
			Expect(k8sClient.Status().Update(ctx, sbx)).To(Succeed())

			// Reconcile should delete the failed sandbox and spawn a replacement
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: poolKey})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.List(ctx, &sbxList, client.InNamespace(namespace), client.MatchingLabels{sandboxesv1alpha1.LabelPoolName: poolName})).To(Succeed())
			// There should be a healthy replacement
			foundHealthy := false
			for _, item := range sbxList.Items {
				if item.Name != sbx.Name {
					foundHealthy = true
					break
				}
			}
			Expect(foundHealthy).To(BeTrue())
		})

		It("should reject SandboxPool with maxWarm < minWarm via CEL validation", func() {
			invalidPool := &sandboxesv1alpha1.SandboxPool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool-cel-invalid",
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.SandboxPoolSpec{
					MinWarm: 5,
					MaxWarm: 2, // Invalid: maxWarm < minWarm
					Template: sandboxesv1alpha1.SandboxTemplateSpec{
						Image: "ghcr.io/hoangndst/buddy-sandboxes-base:latest",
					},
				},
			}
			err := k8sClient.Create(ctx, invalidPool)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("maxWarm must be greater than or equal to minWarm"))
		})

		It("should fence surplus standby deletion against concurrent lease claims using resourceVersion precondition", func() {
			poolName := "test-pool-fenced-deletion"
			sbxName := "test-sbx-fenced-1"

			pool := &sandboxesv1alpha1.SandboxPool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      poolName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.SandboxPoolSpec{
					MinWarm:            1,
					MaxWarm:            3,
					IdleTimeoutSeconds: 15,
					Template: sandboxesv1alpha1.SandboxTemplateSpec{
						Image: "ghcr.io/hoangndst/buddy-sandboxes-base:latest",
					},
				},
			}
			Expect(k8sClient.Create(ctx, pool)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, pool)
			}()

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

			sbx.Status.Phase = sandboxesv1alpha1.SandboxPhaseRunning
			Expect(k8sClient.Status().Update(ctx, sbx)).To(Succeed())

			var currentSbx sandboxesv1alpha1.Sandbox
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbxName, Namespace: namespace}, &currentSbx)).To(Succeed())

			// Mutate sandbox concurrently, incrementing its ResourceVersion
			currentSbx.Labels[sandboxesv1alpha1.LabelPoolStatus] = sandboxesv1alpha1.PoolStatusClaimed
			currentSbx.Labels[sandboxesv1alpha1.LabelLeaseName] = "concurrent-lease"
			Expect(k8sClient.Update(ctx, &currentSbx)).To(Succeed())

			reconciler := &SandboxPoolReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(10),
			}

			// Precondition delete with stale ResourceVersion fails with conflict
			staleSnapshot := currentSbx.DeepCopy()
			staleSnapshot.ResourceVersion = "1"
			readySandboxes := []sandboxesv1alpha1.Sandbox{*staleSnapshot}
			pool.Spec.MinWarm = 0
			now := time.Now().Add(10 * time.Minute)
			remaining, _ := reconciler.recycleSurplusStandbys(ctx, pool, readySandboxes, now)
			Expect(remaining).To(HaveLen(1))

			var fetched sandboxesv1alpha1.Sandbox
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbxName, Namespace: namespace}, &fetched)).To(Succeed())
			Expect(fetched.Labels[sandboxesv1alpha1.LabelPoolStatus]).To(Equal(sandboxesv1alpha1.PoolStatusClaimed))
		})

		It("should schedule reconciliation for pending claim deadline", func() {
			poolName := "test-pool-claim-deadline"
			sbxName := "test-sbx-pending-claim"
			poolKey := types.NamespacedName{Name: poolName, Namespace: namespace}

			pool := &sandboxesv1alpha1.SandboxPool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      poolName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.SandboxPoolSpec{
					MinWarm: 1,
					MaxWarm: 3,
					Template: sandboxesv1alpha1.SandboxTemplateSpec{
						Image: "ghcr.io/hoangndst/buddy-sandboxes-base:latest",
					},
				},
			}
			Expect(k8sClient.Create(ctx, pool)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, pool)
			}()

			boolTrue := true
			deadline := metav1.NewTime(time.Now().Add(12 * time.Second))
			sbx := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sbxName,
					Namespace: namespace,
					Labels: map[string]string{
						sandboxesv1alpha1.LabelPoolName:   poolName,
						sandboxesv1alpha1.LabelPoolStatus: sandboxesv1alpha1.PoolStatusClaimed,
						sandboxesv1alpha1.LabelLeaseName:  "pending-lease-unconfirmed",
					},
					Annotations: map[string]string{
						sandboxesv1alpha1.AnnotationClaimDeadline: deadline.Format(time.RFC3339),
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

			reconciler := &SandboxPoolReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(10),
			}

			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: poolKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
			Expect(result.RequeueAfter).To(BeNumerically("<=", 12*time.Second))

			var fetched sandboxesv1alpha1.Sandbox
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sbxName, Namespace: namespace}, &fetched)).To(Succeed())
		})

		It("should scavenge expired unconfirmed claim when deadline has passed", func() {
			poolName := "test-pool-scavenge-expired"
			sbxName := "test-sbx-expired-claim"
			poolKey := types.NamespacedName{Name: poolName, Namespace: namespace}

			pool := &sandboxesv1alpha1.SandboxPool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      poolName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.SandboxPoolSpec{
					MinWarm: 1,
					MaxWarm: 3,
					Template: sandboxesv1alpha1.SandboxTemplateSpec{
						Image: "ghcr.io/hoangndst/buddy-sandboxes-base:latest",
					},
				},
			}
			Expect(k8sClient.Create(ctx, pool)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, pool)
			}()

			boolTrue := true
			pastDeadline := metav1.NewTime(time.Now().Add(-10 * time.Second))
			sbx := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sbxName,
					Namespace: namespace,
					Labels: map[string]string{
						sandboxesv1alpha1.LabelPoolName:   poolName,
						sandboxesv1alpha1.LabelPoolStatus: sandboxesv1alpha1.PoolStatusClaimed,
						sandboxesv1alpha1.LabelLeaseName:  "dead-lease-never-bound",
					},
					Annotations: map[string]string{
						sandboxesv1alpha1.AnnotationClaimDeadline: pastDeadline.Format(time.RFC3339),
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

			reconciler := &SandboxPoolReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(10),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: poolKey})
			Expect(err).NotTo(HaveOccurred())

			var fetched sandboxesv1alpha1.Sandbox
			err = k8sClient.Get(ctx, types.NamespacedName{Name: sbxName, Namespace: namespace}, &fetched)
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		It("should schedule reconciliation for surplus standby idle recycling", func() {
			poolName := "test-pool-idle-schedule"
			poolKey := types.NamespacedName{Name: poolName, Namespace: namespace}

			pool := &sandboxesv1alpha1.SandboxPool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      poolName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.SandboxPoolSpec{
					MinWarm:            1,
					MaxWarm:            3,
					IdleTimeoutSeconds: 30,
					Template: sandboxesv1alpha1.SandboxTemplateSpec{
						Image: "ghcr.io/hoangndst/buddy-sandboxes-base:latest",
					},
				},
			}
			Expect(k8sClient.Create(ctx, pool)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, pool)
				var list sandboxesv1alpha1.SandboxList
				_ = k8sClient.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabels{sandboxesv1alpha1.LabelPoolName: poolName})
				for _, item := range list.Items {
					_ = k8sClient.Delete(ctx, &item)
				}
			}()

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
				Expect(k8sClient.Status().Update(ctx, sbx)).To(Succeed())
				return sbx
			}

			sbx1 := createStandby("test-sbx-idle-1")
			defer func() { _ = k8sClient.Delete(ctx, sbx1) }()
			sbx2 := createStandby("test-sbx-idle-2")
			defer func() { _ = k8sClient.Delete(ctx, sbx2) }()

			reconciler := &SandboxPoolReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(10),
			}

			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: poolKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0))
			Expect(result.RequeueAfter).To(BeNumerically("<=", 30*time.Second))
		})

		It("should recycle surplus standby runners when idle timeout has elapsed", func() {
			poolName := "test-pool-idle-recycle"

			pool := &sandboxesv1alpha1.SandboxPool{
				ObjectMeta: metav1.ObjectMeta{
					Name:      poolName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.SandboxPoolSpec{
					MinWarm:            1,
					MaxWarm:            3,
					IdleTimeoutSeconds: 15,
					Template: sandboxesv1alpha1.SandboxTemplateSpec{
						Image: "ghcr.io/hoangndst/buddy-sandboxes-base:latest",
					},
				},
			}
			Expect(k8sClient.Create(ctx, pool)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, pool)
			}()

			boolTrue := true
			sbxSurplus := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbx-surplus-recycle",
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
			Expect(k8sClient.Create(ctx, sbxSurplus)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, sbxSurplus) }()

			sbxSurplus.Status.Phase = sandboxesv1alpha1.SandboxPhaseRunning
			Expect(k8sClient.Status().Update(ctx, sbxSurplus)).To(Succeed())

			reconciler := &SandboxPoolReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(10),
			}

			now := time.Now().Add(10 * time.Minute)
			ready := []sandboxesv1alpha1.Sandbox{*sbxSurplus}
			pool.Spec.MinWarm = 0
			remaining, _ := reconciler.recycleSurplusStandbys(ctx, pool, ready, now)
			Expect(remaining).To(BeEmpty())

			var fetched sandboxesv1alpha1.Sandbox
			err := k8sClient.Get(ctx, types.NamespacedName{Name: sbxSurplus.Name, Namespace: namespace}, &fetched)
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		It("should retain claimed runner and propagate error when lease lookup fails transiently", func() {
			pastDeadline := metav1.NewTime(time.Now().Add(-10 * time.Second))
			claimedSbx := sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-transient-lease-runner",
					Namespace: namespace,
					Labels: map[string]string{
						sandboxesv1alpha1.LabelPoolName:   "test-pool",
						sandboxesv1alpha1.LabelPoolStatus: sandboxesv1alpha1.PoolStatusClaimed,
						sandboxesv1alpha1.LabelLeaseName:  "flaky-lease",
					},
					Annotations: map[string]string{
						sandboxesv1alpha1.AnnotationClaimDeadline: pastDeadline.Format(time.RFC3339),
					},
				},
			}

			errLookupClient := &testErrorClient{
				Client: k8sClient,
				getErr: fmt.Errorf("transient etcd failure"),
			}
			reconciler := &SandboxPoolReconciler{
				Client:   errLookupClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(10),
			}

			active, _, err := reconciler.scavengeExpiredClaims(ctx, []sandboxesv1alpha1.Sandbox{claimedSbx}, time.Now())
			Expect(err).To(HaveOccurred())
			Expect(active).To(HaveLen(1))
			Expect(active[0].Name).To(Equal("test-transient-lease-runner"))
		})

		It("should retain claimed runner in capacity count and propagate error when scavenger deletion fails", func() {
			pastDeadline := metav1.NewTime(time.Now().Add(-10 * time.Second))
			claimedSbx := sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-delete-fail-runner",
					Namespace: namespace,
					Labels: map[string]string{
						sandboxesv1alpha1.LabelPoolName:   "test-pool",
						sandboxesv1alpha1.LabelPoolStatus: sandboxesv1alpha1.PoolStatusClaimed,
						sandboxesv1alpha1.LabelLeaseName:  "dead-lease-absent",
					},
					Annotations: map[string]string{
						sandboxesv1alpha1.AnnotationClaimDeadline: pastDeadline.Format(time.RFC3339),
					},
				},
			}

			errDeleteClient := &testErrorClient{
				Client:    k8sClient,
				deleteErr: fmt.Errorf("transient webhook admission failure"),
			}
			reconciler := &SandboxPoolReconciler{
				Client:   errDeleteClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(10),
			}

			active, _, err := reconciler.scavengeExpiredClaims(ctx, []sandboxesv1alpha1.Sandbox{claimedSbx}, time.Now())
			Expect(err).To(HaveOccurred())
			Expect(active).To(HaveLen(1))
			Expect(active[0].Name).To(Equal("test-delete-fail-runner"))
		})

		It("should skip bound runners during claim scavenging", func() {
			boundSbx := sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-bound-runner",
					Namespace: namespace,
					Labels: map[string]string{
						sandboxesv1alpha1.LabelPoolName:   "test-pool",
						sandboxesv1alpha1.LabelPoolStatus: sandboxesv1alpha1.PoolStatusBound,
						sandboxesv1alpha1.LabelLeaseName:  "active-lease",
					},
				},
			}

			reconciler := &SandboxPoolReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(10),
			}

			active, _, err := reconciler.scavengeExpiredClaims(ctx, []sandboxesv1alpha1.Sandbox{boundSbx}, time.Now())
			Expect(err).NotTo(HaveOccurred())
			Expect(active).To(HaveLen(1))
			Expect(active[0].Name).To(Equal("test-bound-runner"))
		})

		It("should retain claimed runner when scavenger deletion encounters conflict from concurrent confirmation", func() {
			pastDeadline := metav1.NewTime(time.Now().Add(-10 * time.Second))
			claimedSbx := sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-conflict-runner",
					Namespace:       namespace,
					ResourceVersion: "12345",
					Labels: map[string]string{
						sandboxesv1alpha1.LabelPoolName:   "test-pool",
						sandboxesv1alpha1.LabelPoolStatus: sandboxesv1alpha1.PoolStatusClaimed,
						sandboxesv1alpha1.LabelLeaseName:  "dead-lease-absent",
					},
					Annotations: map[string]string{
						sandboxesv1alpha1.AnnotationClaimDeadline: pastDeadline.Format(time.RFC3339),
					},
				},
			}

			errConflictClient := &testErrorClient{
				Client:    k8sClient,
				deleteErr: errors.NewConflict(schema.GroupResource{Group: "sandboxes.buddy.io", Resource: "sandboxes"}, "test-conflict-runner", fmt.Errorf("resourceversion conflict")),
			}
			reconciler := &SandboxPoolReconciler{
				Client:   errConflictClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: record.NewFakeRecorder(10),
			}

			active, _, err := reconciler.scavengeExpiredClaims(ctx, []sandboxesv1alpha1.Sandbox{claimedSbx}, time.Now())
			Expect(err).NotTo(HaveOccurred())
			Expect(active).To(HaveLen(1))
			Expect(active[0].Name).To(Equal("test-conflict-runner"))
		})
	})
})

type testErrorClient struct {
	client.Client
	getErr    error
	deleteErr error
}

func (c *testErrorClient) Get(ctx context.Context, key types.NamespacedName, obj client.Object, opts ...client.GetOption) error {
	if c.getErr != nil {
		return c.getErr
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *testErrorClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if c.deleteErr != nil {
		return c.deleteErr
	}
	return c.Client.Delete(ctx, obj, opts...)
}
