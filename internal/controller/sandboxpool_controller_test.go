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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	})
})
