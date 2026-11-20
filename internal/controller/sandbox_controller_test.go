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
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
)

var _ = Describe("Sandbox Controller", func() {
	Context("When reconciling a Sandbox resource", func() {
		const resourceName = "test-agent-sandbox"
		const namespace = "default"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: namespace,
		}

		BeforeEach(func() {
			// Ensure clean state before each test
			sandbox := &sandboxesv1alpha1.Sandbox{}
			err := k8sClient.Get(ctx, typeNamespacedName, sandbox)
			if err == nil {
				_ = k8sClient.Delete(ctx, sandbox)
			}
		})

		AfterEach(func() {
			sandbox := &sandboxesv1alpha1.Sandbox{}
			err := k8sClient.Get(ctx, typeNamespacedName, sandbox)
			if err == nil {
				_ = k8sClient.Delete(ctx, sandbox)
			}
		})

		It("should successfully create Pod, NetworkPolicy, and update conditions", func() {
			deadline := int64(1800)
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image:              "golang:1.25",
					Command:            []string{"/bin/sh", "-c"},
					Args:               []string{"echo hello"},
					MaxDurationSeconds: &deadline,
					Workspace: sandboxesv1alpha1.WorkspaceSpec{
						Ephemeral: true,
						MountPath: "/workspace",
					},
					Network: sandboxesv1alpha1.NetworkSpec{
						AllowInternetWeb:     true,
						AllowDNS:             true,
						BlockClusterInternal: true,
						BlockMetadataService: true,
					},
				},
			}
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())

			reconciler := &SandboxReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// First reconcile: provisions NetworkPolicy and Pod
			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

			// Verify NetworkPolicy was created
			netpol := &networkingv1.NetworkPolicy{}
			netpolKey := types.NamespacedName{Namespace: namespace, Name: fmt.Sprintf("%s-netpol", resourceName)}
			Expect(k8sClient.Get(ctx, netpolKey, netpol)).To(Succeed())
			Expect(netpol.Spec.Ingress).To(BeEmpty())
			Expect(netpol.Spec.Egress).To(HaveLen(2))
			Expect(netpol.OwnerReferences).To(HaveLen(1))
			Expect(netpol.OwnerReferences[0].Name).To(Equal(resourceName))

			// Verify Pod was created with correct security context
			pod := &corev1.Pod{}
			podKey := types.NamespacedName{Namespace: namespace, Name: fmt.Sprintf("%s-sandbox", resourceName)}
			Expect(k8sClient.Get(ctx, podKey, pod)).To(Succeed())
			Expect(pod.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyNever))
			Expect(pod.Spec.ActiveDeadlineSeconds).To(Equal(&deadline))
			Expect(pod.Spec.SecurityContext.FSGroup).NotTo(BeNil())
			Expect(*pod.Spec.SecurityContext.FSGroup).To(Equal(int64(1000)))

			// Verify Container security settings
			Expect(pod.Spec.Containers).To(HaveLen(1))
			container := pod.Spec.Containers[0]
			Expect(container.Image).To(Equal("golang:1.25"))
			Expect(container.WorkingDir).To(Equal("/workspace"))
			Expect(*container.SecurityContext.ReadOnlyRootFilesystem).To(BeTrue())
			Expect(*container.SecurityContext.RunAsNonRoot).To(BeTrue())
			Expect(*container.SecurityContext.RunAsUser).To(Equal(int64(1000)))
			Expect(*container.SecurityContext.AllowPrivilegeEscalation).To(BeFalse())

			// Verify volume mounts: workspace, tmp, home
			mountPaths := []string{}
			for _, m := range container.VolumeMounts {
				mountPaths = append(mountPaths, m.MountPath)
			}
			Expect(mountPaths).To(ContainElements("/workspace", "/tmp", "/home/sandbox"))

			// Verify status after first reconcile
			updatedSandbox := &sandboxesv1alpha1.Sandbox{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updatedSandbox)).To(Succeed())
			Expect(updatedSandbox.Status.Phase).To(Equal(sandboxesv1alpha1.SandboxPhasePending))
			Expect(updatedSandbox.Status.PodName).To(Equal(podKey.Name))
			Expect(updatedSandbox.Status.NetworkPolicyName).To(Equal(netpolKey.Name))

			// Second reconcile: idempotent check
			result2, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(result2.RequeueAfter).To(Equal(time.Duration(0)))
		})

		It("should update Sandbox status to Running when pod enters running phase", func() {
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image: "golang:1.25",
				},
			}
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())

			reconciler := &SandboxReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// Initial reconcile creates the pod
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			// Simulate pod moving to Running
			pod := &corev1.Pod{}
			podKey := types.NamespacedName{Namespace: namespace, Name: fmt.Sprintf("%s-sandbox", resourceName)}
			Expect(k8sClient.Get(ctx, podKey, pod)).To(Succeed())
			pod.Status.Phase = corev1.PodRunning
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

			// Reconcile again to observe pod status
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			updated := &sandboxesv1alpha1.Sandbox{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(sandboxesv1alpha1.SandboxPhaseRunning))
		})

		It("should mark Sandbox as Succeeded with exit code 0 when pod succeeds", func() {
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image: "golang:1.25",
				},
			}
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())

			reconciler := &SandboxReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			// Simulate pod Succeeded
			pod := &corev1.Pod{}
			podKey := types.NamespacedName{Namespace: namespace, Name: fmt.Sprintf("%s-sandbox", resourceName)}
			Expect(k8sClient.Get(ctx, podKey, pod)).To(Succeed())
			pod.Status.Phase = corev1.PodSucceeded
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			updated := &sandboxesv1alpha1.Sandbox{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(sandboxesv1alpha1.SandboxPhaseSucceeded))
			Expect(updated.Status.ExitCode).NotTo(BeNil())
			Expect(*updated.Status.ExitCode).To(Equal(int32(0)))
			Expect(updated.Status.CompletionTime).NotTo(BeNil())
		})

		It("should handle non-existent resource gracefully", func() {
			reconciler := &SandboxReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "non-existent-sandbox",
					Namespace: namespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
		})
	})
})
