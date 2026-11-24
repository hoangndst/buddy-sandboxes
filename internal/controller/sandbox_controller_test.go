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
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
	"k8s.io/client-go/tools/record"
)

type suspensionStatusConflictClient struct {
	client.Client
}

func (c *suspensionStatusConflictClient) Status() client.SubResourceWriter {
	return suspensionStatusConflictWriter{SubResourceWriter: c.Client.Status()}
}

type suspensionStatusConflictWriter struct {
	client.SubResourceWriter
}

func (w suspensionStatusConflictWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if sandbox, ok := obj.(*sandboxesv1alpha1.Sandbox); ok && sandbox.Status.SuspensionPhase == sandboxesv1alpha1.SuspensionPhaseSuspending {
		return apierrors.NewConflict(schema.GroupResource{Group: "sandboxes.buddy.io", Resource: "sandboxes/status"}, sandbox.Name, fmt.Errorf("simulated concurrent spec update"))
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

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
						Ephemeral: ptrBool(true),
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
			Expect(pod.Labels).NotTo(HaveKey("sandboxes.buddy.io/api-broker"))
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

		It("accepts writable persistent workspace suspension and enforces workspace immutability", func() {
			deadline := int64(300)
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image:              "alpine:latest",
					Suspend:            true,
					MaxDurationSeconds: &deadline,
					Workspace:          sandboxesv1alpha1.WorkspaceSpec{Ephemeral: ptrBool(false), Size: "1Gi", MountPath: "/workspace"},
				},
			}
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			defer func() {
				stored := &sandboxesv1alpha1.Sandbox{}
				if k8sClient.Get(ctx, typeNamespacedName, stored) == nil {
					_ = k8sClient.Delete(ctx, stored)
				}
			}()

			stored := &sandboxesv1alpha1.Sandbox{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, stored)).To(Succeed())
			Expect(stored.Spec.Suspend).To(BeTrue())
			Expect(stored.Spec.Workspace.Ephemeral).NotTo(BeNil())
			Expect(*stored.Spec.Workspace.Ephemeral).To(BeFalse())
			stored.Spec.Workspace.Size = "2Gi"
			Expect(apierrors.IsInvalid(k8sClient.Update(ctx, stored))).To(BeTrue())

			stored = &sandboxesv1alpha1.Sandbox{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, stored)).To(Succeed())
			stored.Status.SuspensionPhase = sandboxesv1alpha1.SuspensionPhaseSuspended
			Expect(k8sClient.Status().Update(ctx, stored)).To(Succeed())
		})

		It("defaults an omitted suspend field to false", func() {
			name := resourceName + "-default"
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec:       sandboxesv1alpha1.SandboxSpec{Image: "alpine:latest", Workspace: sandboxesv1alpha1.WorkspaceSpec{Ephemeral: ptrBool(true)}},
			}
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, sandbox) }()
			stored := &sandboxesv1alpha1.Sandbox{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, stored)).To(Succeed())
			Expect(stored.Spec.Suspend).To(BeFalse())
		})

		It("suspends and resumes against the same external PVC with a new Pod generation", func() {
			claimName := resourceName + "-workspace"
			claim := testWorkspaceClaim(claimName)
			claim.Namespace = namespace
			Expect(k8sClient.Create(ctx, claim)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, claim) }()

			deadline := int64(600)
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image:              "alpine:latest",
					MaxDurationSeconds: &deadline,
					Workspace:          sandboxesv1alpha1.WorkspaceSpec{PersistentVolumeClaim: &sandboxesv1alpha1.PersistentVolumeClaimWorkspace{ClaimName: claimName}},
				},
			}
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			r := &SandboxReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			req := reconcile.Request{NamespacedName: typeNamespacedName}
			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			podName := resourceName + "-sandbox"
			firstPod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, firstPod)).To(Succeed())
			firstUID := firstPod.UID
			firstPod.Status.Phase = corev1.PodRunning
			firstPod.Status.StartTime = ptrMetaTime(time.Now())
			Expect(k8sClient.Status().Update(ctx, firstPod)).To(Succeed())
			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())

			stored := &sandboxesv1alpha1.Sandbox{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, stored)).To(Succeed())
			Expect(stored.Status.Phase).To(Equal(sandboxesv1alpha1.SandboxPhaseRunning))
			Expect(stored.Status.FirstPodStartTime).NotTo(BeNil())
			expectUID := string(firstUID)
			Expect(stored.Status.PodUID).To(Equal(expectUID))

			persistedStart := metav1.NewTime(firstPod.Status.StartTime.Time)
			stored.Status.FirstPodStartTime = nil
			Expect(k8sClient.Status().Update(ctx, stored)).To(Succeed())
			Expect(k8sClient.Get(ctx, typeNamespacedName, stored)).To(Succeed())
			stored.Spec.Suspend = true
			Expect(k8sClient.Update(ctx, stored)).To(Succeed())
			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, typeNamespacedName, stored)).To(Succeed())
			Expect(stored.Status.FirstPodStartTime).NotTo(BeNil())
			Expect(stored.Status.FirstPodStartTime.Time).To(Equal(persistedStart.Time))
			Expect(stored.Status.SuspensionPhase).To(Equal(sandboxesv1alpha1.SuspensionPhaseSuspending))
			stillRunningPod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, stillRunningPod)).To(Succeed())
			Expect(stillRunningPod.DeletionTimestamp).To(BeNil())

			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			for i := 0; i < 10; i++ {
				Expect(k8sClient.Get(ctx, typeNamespacedName, stored)).To(Succeed())
				if stored.Status.SuspensionPhase == sandboxesv1alpha1.SuspensionPhaseSuspended {
					break
				}
				_, err = r.Reconcile(ctx, req)
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(stored.Status.SuspensionPhase).To(Equal(sandboxesv1alpha1.SuspensionPhaseSuspended))
			Expect(stored.Status.PodName).To(BeEmpty())
			Expect(stored.Status.PodUID).To(BeEmpty())
			removedPod := &corev1.Pod{}
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, removedPod))).To(BeTrue())
			preservedClaim := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: claimName}, preservedClaim)).To(Succeed())
			Expect(preservedClaim.OwnerReferences).To(BeEmpty())

			stored.Spec.Suspend = false
			Expect(k8sClient.Update(ctx, stored)).To(Succeed())
			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			resumedPod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, resumedPod)).To(Succeed())
			Expect(resumedPod.UID).NotTo(Equal(firstUID))
			var resumedClaimName string
			for _, volume := range resumedPod.Spec.Volumes {
				if volume.Name == "workspace-volume" && volume.PersistentVolumeClaim != nil {
					resumedClaimName = volume.PersistentVolumeClaim.ClaimName
				}
			}
			Expect(resumedClaimName).To(Equal(claimName))
			Expect(resumedPod.Annotations[sandboxesv1alpha1.PodDeadlineSecondsAnnotation]).NotTo(BeEmpty())
			resumedPod.Status.Phase = corev1.PodRunning
			resumedPod.Status.StartTime = ptrMetaTime(time.Now())
			Expect(k8sClient.Status().Update(ctx, resumedPod)).To(Succeed())
			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, typeNamespacedName, stored)).To(Succeed())
			Expect(stored.Status.SuspensionPhase).To(Equal(sandboxesv1alpha1.SuspensionPhaseNone))
			Expect(stored.Status.PodUID).To(Equal(string(resumedPod.UID)))
		})

		It("clears a resume transition when replacement execution terminates", func() {
			for _, outcome := range []struct {
				name     string
				podPhase corev1.PodPhase
				exitCode int32
				phase    sandboxesv1alpha1.SandboxPhase
			}{
				{name: "success", podPhase: corev1.PodSucceeded, exitCode: 0, phase: sandboxesv1alpha1.SandboxPhaseSucceeded},
				{name: "failure", podPhase: corev1.PodFailed, exitCode: 42, phase: sandboxesv1alpha1.SandboxPhaseFailed},
				{name: "running-agent-success", podPhase: corev1.PodRunning, exitCode: 0, phase: sandboxesv1alpha1.SandboxPhaseSucceeded},
				{name: "running-agent-failure", podPhase: corev1.PodRunning, exitCode: 42, phase: sandboxesv1alpha1.SandboxPhaseFailed},
			} {
				name := resourceName + "-resume-terminal-" + outcome.name
				podName := name + "-sandbox"
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "alpine:latest"}}},
				}
				Expect(k8sClient.Create(ctx, pod)).To(Succeed())
				defer func(p *corev1.Pod) { _ = k8sClient.Delete(ctx, p) }(pod)
				pod.Status = corev1.PodStatus{
					Phase: outcome.podPhase,
					ContainerStatuses: []corev1.ContainerStatus{{
						Name: "agent",
						State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
							ExitCode: outcome.exitCode, FinishedAt: metav1.Now(),
						}},
					}},
				}
				Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

				sandbox := &sandboxesv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
					Spec:       sandboxesv1alpha1.SandboxSpec{Image: "alpine:latest"},
				}
				Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
				defer func(s *sandboxesv1alpha1.Sandbox) { _ = k8sClient.Delete(ctx, s) }(sandbox)
				Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, sandbox)).To(Succeed())
				sandbox.Status.Phase = sandboxesv1alpha1.SandboxPhaseRunning
				sandbox.Status.SuspensionPhase = sandboxesv1alpha1.SuspensionPhaseResuming
				sandbox.Status.PodName = podName
				Expect(k8sClient.Status().Update(ctx, sandbox)).To(Succeed())

				r := &SandboxReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
				req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}
				_, err := r.Reconcile(ctx, req)
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Get(ctx, req.NamespacedName, sandbox)).To(Succeed())
				Expect(sandbox.Status.Phase).To(Equal(outcome.phase))
				Expect(sandbox.Status.SuspensionPhase).To(Equal(sandboxesv1alpha1.SuspensionPhaseNone))
				Expect(sandbox.Status.CompletionTime).NotTo(BeNil())
				condition := meta.FindStatusCondition(sandbox.Status.Conditions, sandboxesv1alpha1.ConditionSuspensionReady)
				Expect(condition).NotTo(BeNil())
				Expect(condition.Status).To(Equal(metav1.ConditionFalse))
				Expect(condition.Reason).To(Equal("TerminalExecution"))
				completedAt := sandbox.Status.CompletionTime.DeepCopy()
				uid := pod.UID

				_, err = r.Reconcile(ctx, req)
				Expect(err).NotTo(HaveOccurred())
				Expect(k8sClient.Get(ctx, req.NamespacedName, sandbox)).To(Succeed())
				Expect(sandbox.Status.Phase).To(Equal(outcome.phase))
				Expect(sandbox.Status.SuspensionPhase).To(Equal(sandboxesv1alpha1.SuspensionPhaseNone))
				Expect(sandbox.Status.CompletionTime.Time).To(Equal(completedAt.Time))
				storedPod := &corev1.Pod{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, storedPod)).To(Succeed())
				Expect(storedPod.UID).To(Equal(uid))
			}
		})

		It("observes a completed Pod before applying a racing suspend request", func() {
			name := resourceName + "-completed-suspend"
			podName := name + "-sandbox"
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "alpine:latest"}}},
				Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, pod) }()
			pod.Status.Phase = corev1.PodSucceeded
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Suspend: true,
					Workspace: sandboxesv1alpha1.WorkspaceSpec{
						PersistentVolumeClaim: &sandboxesv1alpha1.PersistentVolumeClaimWorkspace{ClaimName: "retained-workspace"},
					},
				},
			}
			r := &SandboxReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			handled, _, err := r.reconcileSuspension(ctx, sandbox, podName)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeFalse())

			_, err = r.reconcilePod(ctx, sandbox, podName)
			Expect(err).NotTo(HaveOccurred())
			Expect(sandbox.Status.Phase).To(Equal(sandboxesv1alpha1.SandboxPhaseSucceeded))
			storedPod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, storedPod)).To(Succeed())
		})

		It("observes a terminated agent before deleting a Running Pod for suspension", func() {
			for _, initialSuspensionPhase := range []sandboxesv1alpha1.SuspensionPhase{
				sandboxesv1alpha1.SuspensionPhaseNone,
				sandboxesv1alpha1.SuspensionPhaseSuspending,
			} {
				for _, exitCode := range []int32{0, 42} {
					name := fmt.Sprintf("%s-running-agent-exit-%s-%d", resourceName, initialSuspensionPhase, exitCode)
					podName := name + "-sandbox"
					pod := &corev1.Pod{
						ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace},
						Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "alpine:latest"}}},
						Status: corev1.PodStatus{
							Phase: corev1.PodRunning,
							ContainerStatuses: []corev1.ContainerStatus{{
								Name: "agent",
								State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
									ExitCode: exitCode, FinishedAt: metav1.Now(),
								}},
							}},
						},
					}
					fakeClient := fake.NewClientBuilder().WithScheme(k8sClient.Scheme()).WithObjects(pod).Build()
					sandbox := &sandboxesv1alpha1.Sandbox{
						ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
						Spec: sandboxesv1alpha1.SandboxSpec{Image: "alpine:latest", Suspend: true, Workspace: sandboxesv1alpha1.WorkspaceSpec{
							PersistentVolumeClaim: &sandboxesv1alpha1.PersistentVolumeClaimWorkspace{ClaimName: "retained-workspace"},
						}},
						Status: sandboxesv1alpha1.SandboxStatus{
							Phase: sandboxesv1alpha1.SandboxPhaseRunning, SuspensionPhase: initialSuspensionPhase, PodName: podName,
						},
					}
					r := &SandboxReconciler{Client: fakeClient, Scheme: k8sClient.Scheme()}
					handled, _, err := r.reconcileSuspension(ctx, sandbox, podName)
					Expect(err).NotTo(HaveOccurred())
					Expect(handled).To(BeFalse())
					_, err = r.reconcilePod(ctx, sandbox, podName)
					Expect(err).NotTo(HaveOccurred())
					wantPhase := sandboxesv1alpha1.SandboxPhaseFailed
					if exitCode == 0 {
						wantPhase = sandboxesv1alpha1.SandboxPhaseSucceeded
					}
					Expect(sandbox.Status.Phase).To(Equal(wantPhase))
					Expect(sandbox.Status.SuspensionPhase).To(Equal(sandboxesv1alpha1.SuspensionPhaseNone))
					Expect(sandbox.Status.ExitCode).NotTo(BeNil())
					Expect(*sandbox.Status.ExitCode).To(Equal(exitCode))
					condition := meta.FindStatusCondition(sandbox.Status.Conditions, sandboxesv1alpha1.ConditionSuspensionReady)
					Expect(condition).NotTo(BeNil())
					Expect(condition.Status).To(Equal(metav1.ConditionFalse))
					Expect(condition.Reason).To(Equal("TerminalExecution"))
					storedPod := &corev1.Pod{}
					Expect(fakeClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, storedPod)).To(Succeed())
					Expect(storedPod.DeletionTimestamp).To(BeNil())
				}
			}
		})

		It("waits for a terminating Pod even after its stop signal makes it fail", func() {
			name := resourceName + "-terminating-suspend"
			podName := name + "-sandbox"
			deleting := metav1.Now()
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace, DeletionTimestamp: &deleting, Finalizers: []string{"test"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "alpine:latest"}}},
				Status:     corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Terminated"},
			}
			fakeClient := fake.NewClientBuilder().WithScheme(k8sClient.Scheme()).WithObjects(pod).Build()
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: sandboxesv1alpha1.SandboxSpec{Suspend: true, Workspace: sandboxesv1alpha1.WorkspaceSpec{
					PersistentVolumeClaim: &sandboxesv1alpha1.PersistentVolumeClaimWorkspace{ClaimName: "retained-workspace"},
				}},
			}
			r := &SandboxReconciler{Client: fakeClient, Scheme: k8sClient.Scheme()}
			handled, _, err := r.reconcileSuspension(ctx, sandbox, podName)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())
			Expect(sandbox.Status.Phase).To(BeEmpty())
		})

		It("keeps the Pod deletion fence active throughout resume", func() {
			name := resourceName + "-resuming-fence"
			podName := name + "-sandbox"
			deleting := metav1.Now()
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace, DeletionTimestamp: &deleting, Finalizers: []string{"test"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "alpine:latest"}}},
				Status:     corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Terminated"},
			}
			fakeClient := fake.NewClientBuilder().WithScheme(k8sClient.Scheme()).WithObjects(pod).Build()
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Status:     sandboxesv1alpha1.SandboxStatus{SuspensionPhase: sandboxesv1alpha1.SuspensionPhaseResuming},
				Spec: sandboxesv1alpha1.SandboxSpec{Workspace: sandboxesv1alpha1.WorkspaceSpec{
					PersistentVolumeClaim: &sandboxesv1alpha1.PersistentVolumeClaimWorkspace{ClaimName: "retained-workspace"},
				}},
			}
			r := &SandboxReconciler{Client: fakeClient, Scheme: k8sClient.Scheme()}
			handled, _, err := r.reconcileSuspension(ctx, sandbox, podName)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeTrue())
			Expect(sandbox.Status.SuspensionPhase).To(Equal(sandboxesv1alpha1.SuspensionPhaseResuming))
			Expect(sandbox.Status.Phase).To(BeEmpty())
		})

		It("preserves the Pod when resume wins before suspension status is persisted", func() {
			name := resourceName + "-opposite-request"
			podName := name + "-sandbox"
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "alpine:latest"}}},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning},
			}
			fakeClient := fake.NewClientBuilder().WithScheme(k8sClient.Scheme()).WithObjects(pod).Build()
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Status:     sandboxesv1alpha1.SandboxStatus{SuspensionPhase: sandboxesv1alpha1.SuspensionPhaseNone},
				Spec: sandboxesv1alpha1.SandboxSpec{Workspace: sandboxesv1alpha1.WorkspaceSpec{
					PersistentVolumeClaim: &sandboxesv1alpha1.PersistentVolumeClaimWorkspace{ClaimName: "retained-workspace"},
				}},
			}
			r := &SandboxReconciler{Client: fakeClient, Scheme: k8sClient.Scheme()}
			handled, _, err := r.reconcileSuspension(ctx, sandbox, podName)
			Expect(err).NotTo(HaveOccurred())
			Expect(handled).To(BeFalse())
			Expect(sandbox.Status.SuspensionPhase).To(Equal(sandboxesv1alpha1.SuspensionPhaseNone))
			storedPod := &corev1.Pod{}
			Expect(fakeClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, storedPod)).To(Succeed())
			Expect(storedPod.DeletionTimestamp).To(BeNil())
		})

		It("does not delete the Pod when persisting Suspending conflicts", func() {
			name := resourceName + "-suspension-conflict"
			podName := name + "-sandbox"
			claimName := name + "-claim"
			podStart := metav1.NewTime(time.Now().Add(-20 * time.Second))
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "alpine:latest"}}},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning, StartTime: &podStart},
			}
			claim := testWorkspaceClaim(claimName)
			claim.Namespace = namespace
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image: "alpine:latest", Suspend: true,
					Workspace: sandboxesv1alpha1.WorkspaceSpec{PersistentVolumeClaim: &sandboxesv1alpha1.PersistentVolumeClaimWorkspace{ClaimName: claimName}},
				},
				Status: sandboxesv1alpha1.SandboxStatus{
					Phase: sandboxesv1alpha1.SandboxPhaseRunning, SuspensionPhase: sandboxesv1alpha1.SuspensionPhaseNone, PodName: podName,
				},
			}
			baseClient := fake.NewClientBuilder().WithScheme(k8sClient.Scheme()).
				WithStatusSubresource(&sandboxesv1alpha1.Sandbox{}, &corev1.Pod{}).
				WithObjects(pod, claim, sandbox).Build()
			r := &SandboxReconciler{Client: &suspensionStatusConflictClient{Client: baseClient}, Scheme: k8sClient.Scheme()}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}})
			Expect(apierrors.IsConflict(err)).To(BeTrue())
			storedPod := &corev1.Pod{}
			Expect(baseClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, storedPod)).To(Succeed())
			Expect(storedPod.DeletionTimestamp).To(BeNil())
			storedSandbox := &sandboxesv1alpha1.Sandbox{}
			Expect(baseClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, storedSandbox)).To(Succeed())
			Expect(storedSandbox.Status.SuspensionPhase).To(Equal(sandboxesv1alpha1.SuspensionPhaseNone))
			Expect(storedSandbox.Status.FirstPodStartTime).To(BeNil())

			// Simulate resume winning after the failed phase write.
			storedSandbox.Spec.Suspend = false
			Expect(baseClient.Update(ctx, storedSandbox)).To(Succeed())
			r.Client = baseClient
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}})
			Expect(err).NotTo(HaveOccurred())
			Expect(baseClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, storedPod)).To(Succeed())
			Expect(storedPod.DeletionTimestamp).To(BeNil())
		})

		It("waits for Pod disappearance when resume follows persisted Suspending", func() {
			name := resourceName + "-persisted-suspending-resume"
			podName := name + "-sandbox"
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace, Finalizers: []string{"test"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "alpine:latest"}}},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning},
			}
			claimName := name + "-claim"
			claim := testWorkspaceClaim(claimName)
			claim.Namespace = namespace
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: sandboxesv1alpha1.SandboxSpec{Image: "alpine:latest", Workspace: sandboxesv1alpha1.WorkspaceSpec{
					PersistentVolumeClaim: &sandboxesv1alpha1.PersistentVolumeClaimWorkspace{ClaimName: claimName},
				}},
				Status: sandboxesv1alpha1.SandboxStatus{Phase: sandboxesv1alpha1.SandboxPhaseRunning, SuspensionPhase: sandboxesv1alpha1.SuspensionPhaseSuspending, PodName: podName},
			}
			fakeClient := fake.NewClientBuilder().WithScheme(k8sClient.Scheme()).
				WithStatusSubresource(&sandboxesv1alpha1.Sandbox{}, &corev1.Pod{}).
				WithObjects(pod, claim, sandbox).Build()
			r := &SandboxReconciler{Client: fakeClient, Scheme: k8sClient.Scheme()}
			req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}
			_, err := r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			storedSandbox := &sandboxesv1alpha1.Sandbox{}
			Expect(fakeClient.Get(ctx, req.NamespacedName, storedSandbox)).To(Succeed())
			Expect(storedSandbox.Status.SuspensionPhase).To(Equal(sandboxesv1alpha1.SuspensionPhaseResuming))
			storedPod := &corev1.Pod{}
			Expect(fakeClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, storedPod)).To(Succeed())
			Expect(storedPod.DeletionTimestamp).NotTo(BeNil())
			_, err = r.Reconcile(ctx, req)
			Expect(err).NotTo(HaveOccurred())
			Expect(fakeClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, storedPod)).To(Succeed())
			Expect(storedPod.DeletionTimestamp).NotTo(BeNil())
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

		It("rejects IP hosts for credentialed Git repositories at admission", func() {
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "test-git-ip-credential", Namespace: namespace},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image: "alpine:latest",
					Workspace: sandboxesv1alpha1.WorkspaceSpec{
						Git: &sandboxesv1alpha1.GitRepositorySpec{
							URL:       "https://192.0.2.1/team/repo.git",
							SecretRef: &corev1.LocalObjectReference{Name: "git-credentials"},
						},
					},
				},
			}
			err := k8sClient.Create(ctx, sandbox)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("must use a DNS hostname"))
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
						Ephemeral: ptrBool(true),
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
			Expect(envMap["GIT_SECRET_REF"]).To(Equal("git-auth-secret"))

			Expect(gitInit.EnvFrom).To(BeEmpty())
			Expect(gitInit.VolumeMounts).To(ContainElement(HaveField("Name", "git-credentials")))
			Expect(gitInit.VolumeMounts).To(ContainElement(HaveField("ReadOnly", BeTrue())))
			Expect(pod.Spec.Containers[0].VolumeMounts).NotTo(ContainElement(HaveField("Name", "git-credentials")))
			Expect(pod.Spec.Volumes).To(ContainElement(HaveField("Name", "git-credentials")))
			Expect(gitInit.Args[0]).NotTo(ContainSubstring("Cloning repository $GIT_URL"))
			Expect(gitInit.Args[0]).To(ContainSubstring("GIT_CREDENTIAL_PATH"))
		})

		It("projects the Git broker token only into git-init and keeps credentials in memory storage", func() {
			testName := "test-managed-git-credential"
			testKey := types.NamespacedName{Name: testName, Namespace: namespace}
			oldImage, hadImage := os.LookupEnv("BUDDY_GIT_INIT_IMAGE")
			Expect(os.Setenv("BUDDY_GIT_INIT_IMAGE", "buddy-git-init:test")).To(Succeed())
			defer func() {
				if hadImage {
					_ = os.Setenv("BUDDY_GIT_INIT_IMAGE", oldImage)
				} else {
					_ = os.Unsetenv("BUDDY_GIT_INIT_IMAGE")
				}
			}()
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: testName, Namespace: namespace, Annotations: map[string]string{gitCredentialRedemptionAnnotation: "true"}},
				Spec: sandboxesv1alpha1.SandboxSpec{Image: "alpine:latest", Workspace: sandboxesv1alpha1.WorkspaceSpec{
					Ephemeral: ptrBool(true), Git: &sandboxesv1alpha1.GitRepositorySpec{URL: "https://github.com/example/private.git"},
				}},
			}
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(ctx, sandbox)
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: testName + "-sandbox", Namespace: namespace}}
				_ = k8sClient.Delete(ctx, pod)
			}()
			reconciler := &SandboxReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: testKey})
			Expect(err).NotTo(HaveOccurred())
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: testName + "-sandbox", Namespace: namespace}, pod)).To(Succeed())
			Expect(pod.Spec.ServiceAccountName).To(Equal("buddy-sandbox"))
			Expect(pod.Spec.AutomountServiceAccountToken).NotTo(BeNil())
			Expect(*pod.Spec.AutomountServiceAccountToken).To(BeFalse())
			Expect(pod.Labels).To(HaveKeyWithValue(labelGitBrokerKey, "true"))
			Expect(pod.Spec.InitContainers).To(HaveLen(1))
			gitInit := pod.Spec.InitContainers[0]
			Expect(gitInit.Image).To(Equal("buddy-git-init:test"))
			Expect(gitInit.VolumeMounts).To(ContainElement(HaveField("MountPath", "/var/run/secrets/buddy-git-broker")))
			Expect(gitInit.VolumeMounts).To(ContainElement(HaveField("MountPath", "/var/run/buddy-git-runtime")))
			Expect(strings.Join(gitInit.Args, " ")).To(ContainSubstring("--config \"$BROKER_CONFIG\""))
			Expect(strings.Join(gitInit.Args, " ")).NotTo(ContainSubstring("buddy-git-auth"))
			Expect(pod.Spec.Containers[0].VolumeMounts).NotTo(ContainElement(HaveField("Name", "git-broker-token")))
			Expect(pod.Spec.Containers[0].VolumeMounts).NotTo(ContainElement(HaveField("Name", "git-runtime-credentials")))
		})

		It("should inject native MCP sidecars with startup probes, scratch volumes, and discovery env vars", func() {
			testName := "test-mcp-sidecars"
			testKey := types.NamespacedName{Name: testName, Namespace: namespace}
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image: "alpine:latest",
					Workspace: sandboxesv1alpha1.WorkspaceSpec{
						Ephemeral: ptrBool(true),
					},
					MCPServers: []sandboxesv1alpha1.MCPServerSpec{
						{
							Name:           "filesystem",
							Image:          "mcp/filesystem:v1",
							Port:           8080,
							MountWorkspace: true,
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("200m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
							},
						},
						{
							Name:  "memory-db",
							Image: "redis:7-alpine",
							Port:  6379,
							Env: []corev1.EnvVar{
								{Name: "REDIS_PORT", Value: "6379"},
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

			Expect(pod.Spec.InitContainers).To(HaveLen(2))
			fsSidecar := pod.Spec.InitContainers[0]
			Expect(fsSidecar.Name).To(Equal("filesystem"))
			Expect(fsSidecar.Image).To(Equal("mcp/filesystem:v1"))
			Expect(fsSidecar.RestartPolicy).NotTo(BeNil())
			Expect(*fsSidecar.RestartPolicy).To(Equal(corev1.ContainerRestartPolicyAlways))

			Expect(fsSidecar.StartupProbe).NotTo(BeNil())
			Expect(fsSidecar.StartupProbe.TCPSocket).NotTo(BeNil())
			Expect(fsSidecar.StartupProbe.TCPSocket.Port.IntVal).To(Equal(int32(8080)))
			Expect(fsSidecar.StartupProbe.FailureThreshold).To(Equal(int32(30)))

			Expect(fsSidecar.SecurityContext).NotTo(BeNil())
			Expect(*fsSidecar.SecurityContext.RunAsNonRoot).To(BeTrue())
			Expect(*fsSidecar.SecurityContext.RunAsUser).To(Equal(int64(1000)))
			Expect(*fsSidecar.SecurityContext.ReadOnlyRootFilesystem).To(BeTrue())
			Expect(fsSidecar.SecurityContext.Capabilities.Drop).To(ContainElement(corev1.Capability("ALL")))

			Expect(fsSidecar.VolumeMounts).To(ContainElement(corev1.VolumeMount{
				Name:      "tmp-mcp-filesystem",
				MountPath: "/tmp",
			}))
			Expect(fsSidecar.VolumeMounts).To(ContainElement(corev1.VolumeMount{
				Name:      "workspace-volume",
				MountPath: "/workspace",
				ReadOnly:  false,
			}))

			dbSidecar := pod.Spec.InitContainers[1]
			Expect(dbSidecar.Name).To(Equal("memory-db"))
			Expect(dbSidecar.StartupProbe.TCPSocket.Port.IntVal).To(Equal(int32(6379)))
			var dbMountNames []string
			for _, m := range dbSidecar.VolumeMounts {
				dbMountNames = append(dbMountNames, m.Name)
			}
			Expect(dbMountNames).To(ContainElement("tmp-mcp-memory-db"))
			Expect(dbMountNames).NotTo(ContainElement("workspace-volume"))

			agent := pod.Spec.Containers[0]
			Expect(agent.Name).To(Equal("agent"))
			agentEnvMap := make(map[string]string)
			for _, e := range agent.Env {
				agentEnvMap[e.Name] = e.Value
			}
			Expect(agentEnvMap["BUDDY_MCP_FILESYSTEM_URL"]).To(Equal("http://127.0.0.1:8080"))
			Expect(agentEnvMap["BUDDY_MCP_FILESYSTEM_PORT"]).To(Equal("8080"))
			Expect(agentEnvMap["BUDDY_MCP_MEMORY_DB_URL"]).To(Equal("http://127.0.0.1:6379"))
			Expect(agentEnvMap["BUDDY_MCP_MEMORY_DB_PORT"]).To(Equal("6379"))
			Expect(agentEnvMap["BUDDY_MCP_SERVERS"]).To(Equal("filesystem,memory-db"))
		})

		It("should isolate exit codes and succeed when agent exits 0 even if native sidecars are SIGTERM terminated", func() {
			reconciler := &SandboxReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "test-exit-isolation", Namespace: namespace},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image: "alpine:latest",
					MCPServers: []sandboxesv1alpha1.MCPServerSpec{
						{Name: "fs", Image: "mcp/fs:latest", Port: 8080},
					},
				},
			}

			pod := &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodFailed,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "agent",
							State: corev1.ContainerState{
								Terminated: &corev1.ContainerStateTerminated{
									ExitCode: 0,
								},
							},
						},
					},
					InitContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "fs",
							State: corev1.ContainerState{
								Terminated: &corev1.ContainerStateTerminated{
									ExitCode: 143,
								},
							},
						},
					},
				},
			}

			reconciler.observePodStatus(sandbox, pod)

			Expect(sandbox.Status.Phase).To(Equal(sandboxesv1alpha1.SandboxPhaseSucceeded))
			Expect(sandbox.Status.ExitCode).NotTo(BeNil())
			Expect(*sandbox.Status.ExitCode).To(Equal(int32(0)))
			readyCond := meta.FindStatusCondition(sandbox.Status.Conditions, sandboxesv1alpha1.ConditionReady)
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Reason).To(Equal("PodCompleted"))
		})

		It("should record non-zero exit code when agent container fails", func() {
			reconciler := &SandboxReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "test-agent-fail", Namespace: namespace},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image: "alpine:latest",
					MCPServers: []sandboxesv1alpha1.MCPServerSpec{
						{Name: "fs", Image: "mcp/fs:latest", Port: 8080},
					},
				},
			}

			pod := &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodFailed,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "agent",
							State: corev1.ContainerState{
								Terminated: &corev1.ContainerStateTerminated{
									ExitCode: 2,
								},
							},
						},
					},
				},
			}

			reconciler.observePodStatus(sandbox, pod)

			Expect(sandbox.Status.Phase).To(Equal(sandboxesv1alpha1.SandboxPhaseFailed))
			Expect(sandbox.Status.ExitCode).NotTo(BeNil())
			Expect(*sandbox.Status.ExitCode).To(Equal(int32(2)))
		})

		It("should report SidecarDegraded when MCP sidecar enters CrashLoopBackOff mid-flight and recover when ready", func() {
			reconciler := &SandboxReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "test-sidecar-degraded", Namespace: namespace},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image: "alpine:latest",
					MCPServers: []sandboxesv1alpha1.MCPServerSpec{
						{Name: "fs", Image: "mcp/fs:latest", Port: 8080},
					},
				},
			}

			pod := &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "agent",
							State: corev1.ContainerState{
								Running: &corev1.ContainerStateRunning{},
							},
						},
					},
					InitContainerStatuses: []corev1.ContainerStatus{
						{
							Name:  "fs",
							Ready: false,
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason:  "CrashLoopBackOff",
									Message: "back-off 10s restarting failed container",
								},
							},
						},
					},
				},
			}

			reconciler.observePodStatus(sandbox, pod)

			Expect(sandbox.Status.Phase).To(Equal(sandboxesv1alpha1.SandboxPhaseRunning))
			cond := meta.FindStatusCondition(sandbox.Status.Conditions, sandboxesv1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("SidecarDegraded"))

			pod.Status.InitContainerStatuses[0] = corev1.ContainerStatus{
				Name:  "fs",
				Ready: true,
				State: corev1.ContainerState{
					Running: &corev1.ContainerStateRunning{},
				},
			}

			reconciler.observePodStatus(sandbox, pod)

			condRecovered := meta.FindStatusCondition(sandbox.Status.Conditions, sandboxesv1alpha1.ConditionReady)
			Expect(condRecovered).NotTo(BeNil())
			Expect(condRecovered.Status).To(Equal(metav1.ConditionTrue))
			Expect(condRecovered.Reason).To(Equal("PodRunning"))
		})

		It("should ignore native sidecar termination in checkPendingPodErrors but fail on CrashLoopBackOff", func() {
			sandbox := &sandboxesv1alpha1.Sandbox{
				Spec: sandboxesv1alpha1.SandboxSpec{
					MCPServers: []sandboxesv1alpha1.MCPServerSpec{
						{Name: "fs", Image: "mcp/fs:latest", Port: 8080},
					},
				},
			}

			now := metav1.Now()
			status := &sandboxesv1alpha1.SandboxStatus{}

			podInitialTerm := &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
					InitContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "fs",
							State: corev1.ContainerState{
								Terminated: &corev1.ContainerStateTerminated{
									ExitCode: 1,
								},
							},
						},
					},
				},
			}
			failed := checkPendingPodErrors(sandbox, podInitialTerm, now, status)
			Expect(failed).To(BeFalse())

			podCrashLoop := &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
					InitContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "fs",
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason:  "CrashLoopBackOff",
									Message: "CrashLoopBackOff error",
								},
							},
						},
					},
				},
			}
			failed = checkPendingPodErrors(sandbox, podCrashLoop, now, status)
			Expect(failed).To(BeTrue())
			Expect(status.Phase).To(Equal(sandboxesv1alpha1.SandboxPhaseFailed))
			cond := meta.FindStatusCondition(status.Conditions, sandboxesv1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Reason).To(Equal("SidecarStartupFailed"))
		})

		It("should generate bounded scratch volume names conforming to Kubernetes 63-char limits", func() {
			shortName := "fs"
			volShort := mcpScratchVolumeName(shortName)
			Expect(volShort).To(Equal("tmp-mcp-fs"))
			Expect(volShort).To(HaveLen(10))

			maxName := "a123456789b123456789c123456789d123456789e123456789f1234"
			Expect(maxName).To(HaveLen(55))
			volMax := mcpScratchVolumeName(maxName)
			Expect(volMax).To(HaveLen(63))
			Expect(volMax).To(Equal("tmp-mcp-" + maxName))

			longName := "a123456789b123456789c123456789d123456789e123456789f1234extra-characters"
			volLong := mcpScratchVolumeName(longName)
			Expect(volLong).To(HaveLen(63))
			Expect(volLong).To(HavePrefix("tmp-mcp-"))
		})

		It("should reconcile NetworkPolicy and inject proxy env vars when allowedDomains is configured", func() {
			m7Name := "m7-egress-test"
			m7Key := types.NamespacedName{Name: m7Name, Namespace: namespace}
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:        m7Name,
					Namespace:   namespace,
					Annotations: map[string]string{gitCredentialRedemptionAnnotation: "true"},
				},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image:   "alpine:latest",
					Command: []string{"/bin/sh", "-c"},
					Args:    []string{"echo test"},
					Network: sandboxesv1alpha1.NetworkSpec{
						AllowAPIBroker:   true,
						AllowInternetWeb: true,
						AllowDNS:         true,
						AllowedDomains:   []string{"github.com", "*.npmjs.org"},
					},
					Workspace: sandboxesv1alpha1.WorkspaceSpec{
						Ephemeral: ptrBool(true),
						MountPath: "/workspace",
						Git: &sandboxesv1alpha1.GitRepositorySpec{
							URL: "https://github.com/org/repo.git",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, sandbox) }()

			reconciler := &SandboxReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: m7Key})
			Expect(err).NotTo(HaveOccurred())

			// Verify NetworkPolicy
			netpol := &networkingv1.NetworkPolicy{}
			netpolKey := types.NamespacedName{Namespace: namespace, Name: fmt.Sprintf("%s-netpol", m7Name)}
			Expect(k8sClient.Get(ctx, netpolKey, netpol)).To(Succeed())
			Expect(netpol.Spec.Egress).To(HaveLen(4))
			brokerRule := netpol.Spec.Egress[2]
			Expect(brokerRule.Ports).To(HaveLen(1))
			Expect(brokerRule.Ports[0].Port.IntVal).To(Equal(int32(8080)))
			Expect(brokerRule.To).To(HaveLen(1))
			Expect(brokerRule.To[0].NamespaceSelector.MatchLabels).To(HaveKeyWithValue("kubernetes.io/metadata.name", "buddy-sandboxes-system"))
			Expect(brokerRule.To[0].PodSelector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/name", "buddy-api"))
			gitBrokerRule := netpol.Spec.Egress[3]
			Expect(gitBrokerRule.Ports).To(HaveLen(1))
			Expect(gitBrokerRule.Ports[0].Port.IntVal).To(Equal(int32(8080)))
			Expect(gitBrokerRule.To[0].NamespaceSelector.MatchLabels).To(HaveKeyWithValue("kubernetes.io/metadata.name", "buddy-sandboxes-system"))
			Expect(gitBrokerRule.To[0].PodSelector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/name", "buddy-api"))

			// Verify no 0.0.0.0/0 IPBlock rule exists
			for _, egressRule := range netpol.Spec.Egress {
				for _, to := range egressRule.To {
					if to.IPBlock != nil {
						Expect(to.IPBlock.CIDR).NotTo(Equal("0.0.0.0/0"))
					}
				}
			}

			// Verify Pod proxy env vars
			pod := &corev1.Pod{}
			podKey := types.NamespacedName{Namespace: namespace, Name: fmt.Sprintf("%s-sandbox", m7Name)}
			Expect(k8sClient.Get(ctx, podKey, pod)).To(Succeed())
			Expect(pod.Labels).To(HaveKeyWithValue("sandboxes.buddy.io/api-broker", "true"))
			defer func() { _ = k8sClient.Delete(ctx, pod) }()

			var foundHTTPProxy, foundHTTPSProxy, foundNoProxy bool
			for _, env := range pod.Spec.Containers[0].Env {
				if env.Name == "HTTP_PROXY" {
					foundHTTPProxy = true
					Expect(env.Value).To(ContainSubstring("buddy-sandboxes-egress-gateway"))
				}
				if env.Name == "HTTPS_PROXY" {
					foundHTTPSProxy = true
				}
				if env.Name == "NO_PROXY" {
					foundNoProxy = true
					Expect(env.Value).To(ContainSubstring("127.0.0.1"))
				}
			}
			Expect(foundHTTPProxy).To(BeTrue())
			Expect(foundHTTPSProxy).To(BeTrue())
			Expect(foundNoProxy).To(BeTrue())

			// Existing policies are reconciled when their managed egress rules drift.
			netpol.Spec.Egress = nil
			Expect(k8sClient.Update(ctx, netpol)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: m7Key})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, netpolKey, netpol)).To(Succeed())
			Expect(netpol.Spec.Egress).To(HaveLen(4))

			// Verify git-init initContainer has proxy env vars too
			Expect(pod.Spec.InitContainers).To(HaveLen(1))
			var gitInitHasHTTPProxy bool
			for _, env := range pod.Spec.InitContainers[0].Env {
				if env.Name == "HTTP_PROXY" {
					gitInitHasHTTPProxy = true
				}
			}
			Expect(gitInitHasHTTPProxy).To(BeTrue())
		})

		It("should fail pod reconciliation when allowedDomains is configured without an HMAC secret", func() {
			m7MissingSecretName := "m7-missing-secret-test"
			m7MissingSecretKey := types.NamespacedName{Name: m7MissingSecretName, Namespace: namespace}
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      m7MissingSecretName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image:   "alpine:latest",
					Command: []string{"/bin/sh", "-c"},
					Args:    []string{"echo test"},
					Network: sandboxesv1alpha1.NetworkSpec{
						AllowInternetWeb: true,
						AllowDNS:         true,
						AllowedDomains:   []string{"github.com"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, sandbox) }()

			origEnv := os.Getenv("EGRESS_GATEWAY_HMAC_SECRET")
			_ = os.Unsetenv("EGRESS_GATEWAY_HMAC_SECRET")
			defer func() { _ = os.Setenv("EGRESS_GATEWAY_HMAC_SECRET", origEnv) }()

			reconciler := &SandboxReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: m7MissingSecretKey})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("EGRESS_GATEWAY_HMAC_SECRET is required"))
		})

		It("should retain PVC and emit StorageRetained event under default Retain policy", func() {
			sbxName := "retain-pvc-test"
			sbxKey := types.NamespacedName{Name: sbxName, Namespace: namespace}
			ephemeralFalse := false
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sbxName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image:   "alpine:latest",
					Command: []string{"/bin/sh", "-c"},
					Args:    []string{"echo retain"},
					Workspace: sandboxesv1alpha1.WorkspaceSpec{
						Ephemeral: &ephemeralFalse,
						Size:      "1Gi",
					},
				},
			}
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())

			fakeRecorder := record.NewFakeRecorder(10)
			reconciler := &SandboxReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: fakeRecorder,
			}

			// First reconcile adds finalizer
			res, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: sbxKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(res).To(Equal(reconcile.Result{}))

			updated := &sandboxesv1alpha1.Sandbox{}
			Expect(k8sClient.Get(ctx, sbxKey, updated)).To(Succeed())
			Expect(updated.Finalizers).To(ContainElement(workspaceRetentionFinalizer))

			// Second reconcile creates PVC and Pod
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: sbxKey})
			Expect(err).NotTo(HaveOccurred())

			pvcKey := types.NamespacedName{Name: sbxName + "-workspace", Namespace: namespace}
			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, pvcKey, pvc)).To(Succeed())
			Expect(pvc.Labels[labelReclaimPolicyKey]).To(Equal("Retain"))
			Expect(pvc.OwnerReferences).To(BeEmpty())

			// Delete sandbox and reconcile deletion
			Expect(k8sClient.Delete(ctx, updated)).To(Succeed())

			// Third reconcile processes deletion and finalizer
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: sbxKey})
			Expect(err).NotTo(HaveOccurred())

			// Verify StorageRetained event was recorded
			var foundEvent bool
			for len(fakeRecorder.Events) > 0 {
				event := <-fakeRecorder.Events
				if strings.Contains(event, "StorageRetained") {
					foundEvent = true
					break
				}
			}
			Expect(foundEvent).To(BeTrue())

			// PVC must still exist because reclaimPolicy was Retain
			Expect(k8sClient.Get(ctx, pvcKey, pvc)).To(Succeed())
			// Clean up PVC manually
			Expect(k8sClient.Delete(ctx, pvc)).To(Succeed())
		})

		It("should set OwnerReference on PVC and not add finalizer when reclaimPolicy is Delete", func() {
			sbxName := "delete-pvc-test"
			sbxKey := types.NamespacedName{Name: sbxName, Namespace: namespace}
			ephemeralFalse := false
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sbxName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image:   "alpine:latest",
					Command: []string{"/bin/sh", "-c"},
					Args:    []string{"echo delete"},
					Workspace: sandboxesv1alpha1.WorkspaceSpec{
						Ephemeral:     &ephemeralFalse,
						Size:          "1Gi",
						ReclaimPolicy: sandboxesv1alpha1.WorkspaceReclaimDelete,
					},
				},
			}
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, sandbox) }()

			fakeRecorder := record.NewFakeRecorder(10)
			reconciler := &SandboxReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: fakeRecorder,
			}

			// Reconcile should not add retention finalizer
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: sbxKey})
			Expect(err).NotTo(HaveOccurred())

			updated := &sandboxesv1alpha1.Sandbox{}
			Expect(k8sClient.Get(ctx, sbxKey, updated)).To(Succeed())
			Expect(updated.Finalizers).NotTo(ContainElement(workspaceRetentionFinalizer))

			// PVC has OwnerReference pointing to sandbox
			pvcKey := types.NamespacedName{Name: sbxName + "-workspace", Namespace: namespace}
			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, pvcKey, pvc)).To(Succeed())
			Expect(pvc.OwnerReferences).NotTo(BeEmpty())
			Expect(pvc.OwnerReferences[0].Name).To(Equal(sbxName))
			_ = k8sClient.Delete(ctx, pvc)
		})

		It("should emit Created and Completed events during pod lifecycle", func() {
			sbxName := "event-lifecycle-test"
			sbxKey := types.NamespacedName{Name: sbxName, Namespace: namespace}
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sbxName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image:   "alpine:latest",
					Command: []string{"/bin/sh", "-c"},
					Args:    []string{"echo events"},
				},
			}
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, sandbox) }()

			fakeRecorder := record.NewFakeRecorder(10)
			reconciler := &SandboxReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: fakeRecorder,
			}

			// First reconcile creates the pod and emits Created event
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: sbxKey})
			Expect(err).NotTo(HaveOccurred())

			var createdEvent bool
			for len(fakeRecorder.Events) > 0 {
				event := <-fakeRecorder.Events
				if strings.Contains(event, "Created") && strings.Contains(event, "Sandbox pod created") {
					createdEvent = true
					break
				}
			}
			Expect(createdEvent).To(BeTrue())

			// Simulate pod completed with exit code 0
			podKey := types.NamespacedName{Name: sbxName + "-sandbox", Namespace: namespace}
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, podKey, pod)).To(Succeed())
			zero := int32(0)
			pod.Status.Phase = corev1.PodSucceeded
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{
				{
					Name: "agent",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: zero,
						},
					},
				},
			}
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

			// Second reconcile observes pod completed and emits Completed event
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: sbxKey})
			Expect(err).NotTo(HaveOccurred())

			var completedEvent bool
			for len(fakeRecorder.Events) > 0 {
				event := <-fakeRecorder.Events
				if strings.Contains(event, "Completed") {
					completedEvent = true
					break
				}
			}
			Expect(completedEvent).To(BeTrue())
		})

		It("should detach existing OwnerReference from pre-existing PVC under Retain policy", func() {
			sbxName := "detach-pvc-owner-test"
			sbxKey := types.NamespacedName{Name: sbxName, Namespace: namespace}
			ephemeralFalse := false
			sandbox := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sbxName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.SandboxSpec{
					Image:   "alpine:latest",
					Command: []string{"/bin/sh", "-c"},
					Args:    []string{"echo detach"},
					Workspace: sandboxesv1alpha1.WorkspaceSpec{
						Ephemeral: &ephemeralFalse,
						Size:      "1Gi",
					},
				},
			}
			Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())

			// Pre-create PVC with an OwnerReference pointing to sandbox (simulating prior version)
			pvcKey := types.NamespacedName{Name: sbxName + "-workspace", Namespace: namespace}
			isController := true
			pvc := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sbxName + "-workspace",
					Namespace: namespace,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "sandboxes.buddy.io/v1alpha1",
							Kind:       "Sandbox",
							Name:       sbxName,
							UID:        sandbox.UID,
							Controller: &isController,
						},
					},
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

			fakeRecorder := record.NewFakeRecorder(10)
			reconciler := &SandboxReconciler{
				Client:   k8sClient,
				Scheme:   k8sClient.Scheme(),
				Recorder: fakeRecorder,
			}

			// First reconcile adds finalizer
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: sbxKey})
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile reconciles storage and should strip OwnerReference from existing PVC
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: sbxKey})
			Expect(err).NotTo(HaveOccurred())

			existingPVC := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, pvcKey, existingPVC)).To(Succeed())
			Expect(existingPVC.OwnerReferences).To(BeEmpty())
			Expect(existingPVC.Labels[labelReclaimPolicyKey]).To(Equal("Retain"))

			// Now test deletion: even if an owner reference were re-added, reconcileDelete strips it
			existingPVC.OwnerReferences = []metav1.OwnerReference{
				{
					APIVersion: "sandboxes.buddy.io/v1alpha1",
					Kind:       "Sandbox",
					Name:       sbxName,
					UID:        sandbox.UID,
				},
			}
			Expect(k8sClient.Update(ctx, existingPVC)).To(Succeed())

			Expect(k8sClient.Delete(ctx, sandbox)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: sbxKey})
			Expect(err).NotTo(HaveOccurred())

			// PVC must still exist and have owner reference removed
			Expect(k8sClient.Get(ctx, pvcKey, existingPVC)).To(Succeed())
			Expect(existingPVC.OwnerReferences).To(BeEmpty())

			_ = k8sClient.Delete(ctx, existingPVC)
		})
	})
})
