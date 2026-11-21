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
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
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
			pod := &corev1.Pod{}
			podKey := types.NamespacedName{Namespace: namespace, Name: fmt.Sprintf("%s-sandbox", resourceName)}
			if err := k8sClient.Get(ctx, podKey, pod); err == nil {
				_ = k8sClient.Delete(ctx, pod)
			}
		})

		AfterEach(func() {
			sandbox := &sandboxesv1alpha1.Sandbox{}
			err := k8sClient.Get(ctx, typeNamespacedName, sandbox)
			if err == nil {
				_ = k8sClient.Delete(ctx, sandbox)
			}
			pod := &corev1.Pod{}
			podKey := types.NamespacedName{Namespace: namespace, Name: fmt.Sprintf("%s-sandbox", resourceName)}
			if err := k8sClient.Get(ctx, podKey, pod); err == nil {
				_ = k8sClient.Delete(ctx, pod)
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
			Expect(netpol.Spec.Egress[0].To).To(HaveLen(1))
			Expect(netpol.Spec.Egress[0].To[0].NamespaceSelector.MatchLabels).To(HaveKeyWithValue("kubernetes.io/metadata.name", "kube-system"))
			Expect(netpol.Spec.Egress[0].To[0].PodSelector.MatchLabels).To(HaveKeyWithValue("k8s-app", "kube-dns"))
			Expect(netpol.OwnerReferences).To(HaveLen(1))
			Expect(netpol.OwnerReferences[0].Name).To(Equal(resourceName))

			// Verify Pod was created with correct security context
			pod := &corev1.Pod{}
			podKey := types.NamespacedName{Namespace: namespace, Name: fmt.Sprintf("%s-sandbox", resourceName)}
			Expect(k8sClient.Get(ctx, podKey, pod)).To(Succeed())
			Expect(pod.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyNever))
			Expect(pod.Spec.ActiveDeadlineSeconds).To(Equal(&deadline))
			Expect(pod.Spec.ShareProcessNamespace).NotTo(BeNil())
			Expect(*pod.Spec.ShareProcessNamespace).To(BeFalse())
			Expect(pod.Annotations).To(HaveKeyWithValue(sandboxesv1alpha1.InitialMaxDurationAnnotation, "1800"))
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
			envByName := make(map[string]corev1.EnvVar, len(container.Env))
			for _, env := range container.Env {
				envByName[env.Name] = env
			}
			Expect(envByName).To(HaveKey(sandboxesv1alpha1.AgentEnvSandboxName))
			Expect(envByName[sandboxesv1alpha1.AgentEnvSandboxName].ValueFrom.FieldRef.FieldPath).To(Equal("metadata.labels['sandboxes.buddy.io/sandbox']"))
			Expect(envByName[sandboxesv1alpha1.AgentEnvPodUID].ValueFrom.FieldRef.FieldPath).To(Equal("metadata.uid"))
			Expect(envByName[sandboxesv1alpha1.AgentEnvCPURequestMillicores].ValueFrom.ResourceFieldRef.Divisor).To(Equal(resource.MustParse("1m")))
			Expect(envByName[sandboxesv1alpha1.AgentEnvMemoryLimitBytes].ValueFrom.ResourceFieldRef.Divisor).To(Equal(resource.MustParse("1")))
			Expect(envByName[sandboxesv1alpha1.AgentEnvInitialMaxDurationSeconds].ValueFrom.FieldRef.FieldPath).To(Equal("metadata.annotations['sandboxes.buddy.io/initial-max-duration-seconds']"))

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

		It("should halt pod creation and set RuntimeClassUnavailable when RuntimeClass is missing", func() {
			testName := "test-runtime-missing"
			testKey := types.NamespacedName{Name: testName, Namespace: namespace}
			missingRuntime := "non-existent-runtime"
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image:            "golang:1.25",
					RuntimeClassName: &missingRuntime,
				},
			}
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, sandbox)
			}()

			reconciler := &SandboxReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: testKey})
			Expect(err).NotTo(HaveOccurred())

			// Verify Pod was NOT created
			pod := &corev1.Pod{}
			podKey := types.NamespacedName{Namespace: namespace, Name: fmt.Sprintf("%s-sandbox", testName)}
			err = k8sClient.Get(ctx, podKey, pod)
			Expect(err).To(HaveOccurred())

			// Verify status condition is RuntimeClassUnavailable
			updated := &sandboxesv1alpha1.Sandbox{}
			Expect(k8sClient.Get(ctx, testKey, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(sandboxesv1alpha1.SandboxPhasePending))
			cond := meta.FindStatusCondition(updated.Status.Conditions, sandboxesv1alpha1.ConditionRuntimeClassReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(sandboxesv1alpha1.ReasonRuntimeClassUnavailable))
		})

		It("should validate RuntimeClass and create Pod with hardened security context when RuntimeClass exists", func() {
			testName := "test-runtime-valid"
			testKey := types.NamespacedName{Name: testName, Namespace: namespace}
			runtimeName := "kata-test-runtime"
			rc := &nodev1.RuntimeClass{
				ObjectMeta: metav1.ObjectMeta{
					Name: runtimeName,
				},
				Handler: "kata",
			}
			_ = k8sClient.Create(ctx, rc)
			defer func() {
				_ = k8sClient.Delete(ctx, rc)
			}()

			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image:            "golang:1.25",
					RuntimeClassName: &runtimeName,
				},
			}
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, sandbox)
				pod := &corev1.Pod{}
				pod.Namespace = namespace
				pod.Name = fmt.Sprintf("%s-sandbox", testName)
				_ = k8sClient.Delete(ctx, pod)
			}()

			reconciler := &SandboxReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: testKey})
			Expect(err).NotTo(HaveOccurred())

			// Verify Pod was created with runtimeClassName
			pod := &corev1.Pod{}
			podKey := types.NamespacedName{Namespace: namespace, Name: fmt.Sprintf("%s-sandbox", testName)}
			Expect(k8sClient.Get(ctx, podKey, pod)).To(Succeed())
			Expect(pod.Spec.RuntimeClassName).NotTo(BeNil())
			Expect(*pod.Spec.RuntimeClassName).To(Equal(runtimeName))

			// Verify PodSecurityContext hardening
			Expect(pod.Spec.SecurityContext).NotTo(BeNil())
			Expect(pod.Spec.SecurityContext.RunAsNonRoot).NotTo(BeNil())
			Expect(*pod.Spec.SecurityContext.RunAsNonRoot).To(BeTrue())
			Expect(pod.Spec.SecurityContext.RunAsUser).NotTo(BeNil())
			Expect(*pod.Spec.SecurityContext.RunAsUser).To(Equal(int64(1000)))
			Expect(pod.Spec.SecurityContext.SeccompProfile).NotTo(BeNil())
			Expect(pod.Spec.SecurityContext.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))

			// Verify container security context hardening
			Expect(pod.Spec.Containers).To(HaveLen(1))
			agentContainer := pod.Spec.Containers[0]
			Expect(agentContainer.SecurityContext).NotTo(BeNil())
			Expect(agentContainer.SecurityContext.AllowPrivilegeEscalation).NotTo(BeNil())
			Expect(*agentContainer.SecurityContext.AllowPrivilegeEscalation).To(BeFalse())
			Expect(agentContainer.SecurityContext.ReadOnlyRootFilesystem).NotTo(BeNil())
			Expect(*agentContainer.SecurityContext.ReadOnlyRootFilesystem).To(BeTrue())
			Expect(agentContainer.SecurityContext.RunAsNonRoot).NotTo(BeNil())
			Expect(*agentContainer.SecurityContext.RunAsNonRoot).To(BeTrue())
			Expect(agentContainer.SecurityContext.RunAsUser).NotTo(BeNil())
			Expect(*agentContainer.SecurityContext.RunAsUser).To(Equal(int64(1000)))
			Expect(agentContainer.SecurityContext.Capabilities).NotTo(BeNil())
			Expect(agentContainer.SecurityContext.Capabilities.Drop).To(ContainElement(corev1.Capability("ALL")))
			Expect(agentContainer.SecurityContext.SeccompProfile).NotTo(BeNil())
			Expect(agentContainer.SecurityContext.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))

			// Verify condition RuntimeClassReady is True
			updated := &sandboxesv1alpha1.Sandbox{}
			Expect(k8sClient.Get(ctx, testKey, updated)).To(Succeed())
			cond := meta.FindStatusCondition(updated.Status.Conditions, sandboxesv1alpha1.ConditionRuntimeClassReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal(sandboxesv1alpha1.ReasonRuntimeClassValid))
		})

		It("should detect init-container failures and mark Sandbox as Failed", func() {
			testName := "test-init-fail"
			testKey := types.NamespacedName{Name: testName, Namespace: namespace}
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image: "golang:1.25",
				},
			}
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, sandbox)
				pod := &corev1.Pod{}
				pod.Namespace = namespace
				pod.Name = fmt.Sprintf("%s-sandbox", testName)
				_ = k8sClient.Delete(ctx, pod)
			}()

			reconciler := &SandboxReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: testKey})
			Expect(err).NotTo(HaveOccurred())

			// Simulate init-container crash loop
			pod := &corev1.Pod{}
			podKey := types.NamespacedName{Namespace: namespace, Name: fmt.Sprintf("%s-sandbox", testName)}
			Expect(k8sClient.Get(ctx, podKey, pod)).To(Succeed())
			pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
				{
					Name: "git-init",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason:  "CrashLoopBackOff",
							Message: "Back-off 10s restarting failed container",
						},
					},
				},
			}
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: testKey})
			Expect(err).NotTo(HaveOccurred())

			updated := &sandboxesv1alpha1.Sandbox{}
			Expect(k8sClient.Get(ctx, testKey, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(sandboxesv1alpha1.SandboxPhaseFailed))
			cond := meta.FindStatusCondition(updated.Status.Conditions, sandboxesv1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("CrashLoopBackOff"))
		})

		It("should surface scheduler Unschedulable condition on Sandbox", func() {
			testName := "test-unschedulable"
			testKey := types.NamespacedName{Name: testName, Namespace: namespace}
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image: "golang:1.25",
				},
			}
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, sandbox)
				pod := &corev1.Pod{}
				pod.Namespace = namespace
				pod.Name = fmt.Sprintf("%s-sandbox", testName)
				_ = k8sClient.Delete(ctx, pod)
			}()

			reconciler := &SandboxReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: testKey})
			Expect(err).NotTo(HaveOccurred())

			// Simulate pod Unschedulable condition
			pod := &corev1.Pod{}
			podKey := types.NamespacedName{Namespace: namespace, Name: fmt.Sprintf("%s-sandbox", testName)}
			Expect(k8sClient.Get(ctx, podKey, pod)).To(Succeed())
			pod.Status.Conditions = []corev1.PodCondition{
				{
					Type:    corev1.PodScheduled,
					Status:  corev1.ConditionFalse,
					Reason:  "Unschedulable",
					Message: "0/1 nodes are available: 1 node has taint that the pod didn't tolerate",
				},
			}
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: testKey})
			Expect(err).NotTo(HaveOccurred())

			updated := &sandboxesv1alpha1.Sandbox{}
			Expect(k8sClient.Get(ctx, testKey, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(sandboxesv1alpha1.SandboxPhasePending))
			cond := meta.FindStatusCondition(updated.Status.Conditions, sandboxesv1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("Unschedulable"))
			Expect(cond.Message).To(ContainSubstring("0/1 nodes are available"))
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

		It("should attach external PVC without OwnerReference and configure workspace volume", func() {
			pvcName := "pre-existing-user-pvc"
			pvc := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      pvcName,
					Namespace: namespace,
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("5Gi"),
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, pvc) }()

			testName := "test-ext-pvc"
			testKey := types.NamespacedName{Name: testName, Namespace: namespace}
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image: "alpine:latest",
					Workspace: sandboxesv1alpha1.WorkspaceSpec{
						PersistentVolumeClaim: &sandboxesv1alpha1.PersistentVolumeClaimWorkspace{
							ClaimName: pvcName,
							ReadOnly:  true,
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, sandbox)
				pod := &corev1.Pod{}
				pod.Namespace = namespace
				pod.Name = fmt.Sprintf("%s-sandbox", testName)
				_ = k8sClient.Delete(ctx, pod)
			}()

			reconciler := &SandboxReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: testKey})
			Expect(err).NotTo(HaveOccurred())

			// 1. Verify PVC condition
			updated := &sandboxesv1alpha1.Sandbox{}
			Expect(k8sClient.Get(ctx, testKey, updated)).To(Succeed())
			cond := meta.FindStatusCondition(updated.Status.Conditions, sandboxesv1alpha1.ConditionStorageConfigured)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("ExternalPVCAttached"))
			Expect(updated.Status.PVCName).To(Equal(pvcName))

			// 2. Verify external PVC does NOT have owner reference attached
			existingPVC := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: pvcName}, existingPVC)).To(Succeed())
			Expect(existingPVC.OwnerReferences).To(BeEmpty())

			// 3. Verify Pod spec volume and volume mount
			pod := &corev1.Pod{}
			podKey := types.NamespacedName{Namespace: namespace, Name: fmt.Sprintf("%s-sandbox", testName)}
			Expect(k8sClient.Get(ctx, podKey, pod)).To(Succeed())

			var wsVol *corev1.Volume
			for i := range pod.Spec.Volumes {
				if pod.Spec.Volumes[i].Name == "workspace-volume" {
					wsVol = &pod.Spec.Volumes[i]
					break
				}
			}
			Expect(wsVol).NotTo(BeNil())
			Expect(wsVol.PersistentVolumeClaim).NotTo(BeNil())
			Expect(wsVol.PersistentVolumeClaim.ClaimName).To(Equal(pvcName))
			Expect(wsVol.PersistentVolumeClaim.ReadOnly).To(BeTrue())

			// Volume mount is read-only
			Expect(pod.Spec.Containers[0].VolumeMounts[0].ReadOnly).To(BeTrue())
		})

		It("should persist a missing external PVC condition", func() {
			testName := "test-missing-ext-pvc"
			testKey := types.NamespacedName{Name: testName, Namespace: namespace}
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: namespace},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image: "alpine:latest",
					Workspace: sandboxesv1alpha1.WorkspaceSpec{
						PersistentVolumeClaim: &sandboxesv1alpha1.PersistentVolumeClaimWorkspace{ClaimName: "missing-user-pvc"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, sandbox) }()

			reconciler := &SandboxReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: testKey})
			Expect(err).To(HaveOccurred())

			updated := &sandboxesv1alpha1.Sandbox{}
			Expect(k8sClient.Get(ctx, testKey, updated)).To(Succeed())
			condition := meta.FindStatusCondition(updated.Status.Conditions, sandboxesv1alpha1.ConditionStorageConfigured)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal("PVCNotFound"))
			Expect(updated.Status.Phase).To(Equal(sandboxesv1alpha1.SandboxPhasePending))
		})

		It("should inject git-init container when git repository seeding is configured", func() {
			testName := "test-git-seed"
			testKey := types.NamespacedName{Name: testName, Namespace: namespace}
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image: "alpine:latest",
					Workspace: sandboxesv1alpha1.WorkspaceSpec{
						Ephemeral: true,
						Git: &sandboxesv1alpha1.GitRepositorySpec{
							URL:      "https://github.com/example/sample-repo.git",
							Revision: "v1.2.3",
							Depth:    1,
							Subpath:  "repo",
							SecretRef: &corev1.LocalObjectReference{
								Name: "git-auth-secret",
							},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, sandbox)
				pod := &corev1.Pod{}
				pod.Namespace = namespace
				pod.Name = fmt.Sprintf("%s-sandbox", testName)
				_ = k8sClient.Delete(ctx, pod)
			}()

			reconciler := &SandboxReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: testKey})
			Expect(err).NotTo(HaveOccurred())

			pod := &corev1.Pod{}
			podKey := types.NamespacedName{Namespace: namespace, Name: fmt.Sprintf("%s-sandbox", testName)}
			Expect(k8sClient.Get(ctx, podKey, pod)).To(Succeed())

			Expect(pod.Spec.InitContainers).To(HaveLen(1))
			gitInit := pod.Spec.InitContainers[0]
			Expect(gitInit.Name).To(Equal("git-init"))
			Expect(gitInit.Image).To(Equal("alpine/git:latest"))
			Expect(gitInit.Args).To(ContainElement(ContainSubstring("safe.directory \"$TARGET\"")))
			Expect(gitInit.SecurityContext).NotTo(BeNil())
			Expect(*gitInit.SecurityContext.RunAsNonRoot).To(BeTrue())
			Expect(*gitInit.SecurityContext.RunAsUser).To(Equal(int64(1000)))

			// Check environment variables
			envMap := make(map[string]string)
			for _, e := range gitInit.Env {
				envMap[e.Name] = e.Value
			}
			Expect(envMap["GIT_URL"]).To(Equal("https://github.com/example/sample-repo.git"))
			Expect(envMap["GIT_REVISION"]).To(Equal("v1.2.3"))
			Expect(envMap["GIT_DEPTH"]).To(Equal("1"))
			Expect(envMap["GIT_SUBPATH"]).To(Equal("repo"))

			// Check SecretRef envFrom
			Expect(gitInit.EnvFrom).To(HaveLen(1))
			Expect(gitInit.EnvFrom[0].SecretRef.Name).To(Equal("git-auth-secret"))
		})
	})
})
