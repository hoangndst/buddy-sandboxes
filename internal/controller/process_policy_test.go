package controller

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
)

func testWorkspaceClaim(name string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("100Mi")}},
		},
	}
}

func ptrBool(value bool) *bool {
	return &value
}

var _ = Describe("Process namespace and deadline policy", func() {
	It("clones into an empty workspace and preserves valid or invalid retained contents", func() {
		if _, err := exec.LookPath("git"); err != nil {
			Skip("git executable is unavailable")
		}
		root := GinkgoT().TempDir()
		source := filepath.Join(root, "source")
		Expect(os.MkdirAll(source, 0o755)).To(Succeed())
		mustRun := func(args ...string) {
			command := exec.Command("git", args...)
			command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
			output, err := command.CombinedOutput()
			Expect(err).NotTo(HaveOccurred(), string(output))
		}
		mustRun("init", "-b", "main", source)
		Expect(os.WriteFile(filepath.Join(source, "tracked.txt"), []byte("initial\n"), 0o600)).To(Succeed())
		mustRun("-C", source, "add", "tracked.txt")
		mustRun("-C", source, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-m", "initial")

		runInit := func(target string) error {
			home := filepath.Join(root, "home-"+filepath.Base(target))
			if err := os.MkdirAll(home, 0o755); err != nil {
				return err
			}
			cmd := exec.Command("sh", "-c", gitCloneScript)
			cmd.Env = append(os.Environ(),
				"HOME="+home,
				"WORKSPACE_MOUNT="+target,
				"GIT_URL="+source,
				"GIT_REVISION=",
				"GIT_DEPTH=",
				"GIT_SUBPATH=",
			)
			output, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("git-init failed: %w: %s", err, output)
			}
			return nil
		}

		emptyTarget := filepath.Join(root, "empty-workspace")
		Expect(runInit(emptyTarget)).To(Succeed())
		cloned, err := os.ReadFile(filepath.Join(emptyTarget, "tracked.txt"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(cloned)).To(Equal("initial\n"))

		Expect(os.WriteFile(filepath.Join(emptyTarget, "tracked.txt"), []byte("user edit\n"), 0o600)).To(Succeed())
		mustRun("-C", emptyTarget, "remote", "set-url", "origin", "https://example.invalid/changed.git")
		Expect(runInit(emptyTarget)).To(Succeed())
		preserved, err := os.ReadFile(filepath.Join(emptyTarget, "tracked.txt"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(preserved)).To(Equal("user edit\n"))
		remote, err := exec.Command("git", "-C", emptyTarget, "remote", "get-url", "origin").Output()
		Expect(err).NotTo(HaveOccurred())
		Expect(string(remote)).To(Equal("https://example.invalid/changed.git\n"))

		invalidTarget := filepath.Join(root, "nonempty-invalid")
		Expect(os.MkdirAll(invalidTarget, 0o755)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(invalidTarget, "keep.txt"), []byte("keep me\n"), 0o600)).To(Succeed())
		err = runInit(invalidTarget)
		Expect(err).To(HaveOccurred())
		unchanged, readErr := os.ReadFile(filepath.Join(invalidTarget, "keep.txt"))
		Expect(readErr).NotTo(HaveOccurred())
		Expect(string(unchanged)).To(Equal("keep me\n"))
	})

	It("builds opt-in shared PID Pods with Downward API fields and a zero duration snapshot", func() {
		share := true
		sandbox := &sandboxesv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: "process-policy-opt-in", Namespace: "default"},
			Spec:       sandboxesv1alpha1.SandboxSpec{Image: "alpine:latest", ShareProcessNamespace: &share},
		}
		pod := (&SandboxReconciler{}).buildSandboxPod(sandbox, "process-policy-opt-in-sandbox")
		Expect(pod.Spec.ShareProcessNamespace).NotTo(BeNil())
		Expect(*pod.Spec.ShareProcessNamespace).To(BeTrue())
		Expect(pod.Annotations).To(HaveKeyWithValue(sandboxesv1alpha1.InitialMaxDurationAnnotation, "0"))
		container := pod.Spec.Containers[0]
		env := make(map[string]corev1.EnvVar, len(container.Env))
		for _, variable := range container.Env {
			env[variable.Name] = variable
		}
		Expect(env[sandboxesv1alpha1.AgentEnvPodName].ValueFrom.FieldRef.FieldPath).To(Equal("metadata.name"))
		Expect(env[sandboxesv1alpha1.AgentEnvPodNamespace].ValueFrom.FieldRef.FieldPath).To(Equal("metadata.namespace"))
		Expect(env[sandboxesv1alpha1.AgentEnvPodUID].ValueFrom.FieldRef.FieldPath).To(Equal("metadata.uid"))
		Expect(env[sandboxesv1alpha1.AgentEnvCPURequestMillicores].ValueFrom.ResourceFieldRef.Divisor.String()).To(Equal("1m"))
		Expect(env[sandboxesv1alpha1.AgentEnvMemoryRequestBytes].ValueFrom.ResourceFieldRef.Divisor.String()).To(Equal("1"))
		Expect(env[sandboxesv1alpha1.AgentEnvInitialMaxDurationSeconds].ValueFrom.FieldRef.FieldPath).To(ContainSubstring(sandboxesv1alpha1.InitialMaxDurationAnnotation))
	})

	It("applies only legal monotonic deadline changes", func() {
		ptr := func(value int64) *int64 { return &value }
		cases := []struct {
			name       string
			desired    *int64
			actual     *int64
			wantReady  bool
			wantActual *int64
			wantReason string
		}{
			{name: "nil matches nil", wantReady: true, wantReason: sandboxesv1alpha1.ReasonDeadlinePolicyMatched},
			{name: "set missing deadline", desired: ptr(60), wantReady: true, wantActual: ptr(60), wantReason: sandboxesv1alpha1.ReasonDeadlinePolicyPatched},
			{name: "tighten positive deadline", desired: ptr(20), actual: ptr(60), wantReady: true, wantActual: ptr(20), wantReason: sandboxesv1alpha1.ReasonDeadlinePolicyPatched},
			{name: "equal deadline", desired: ptr(60), actual: ptr(60), wantReady: true, wantActual: ptr(60), wantReason: sandboxesv1alpha1.ReasonDeadlinePolicyMatched},
			{name: "cannot increase deadline", desired: ptr(60), actual: ptr(20), wantReady: false, wantActual: ptr(20), wantReason: sandboxesv1alpha1.ReasonDeadlinePolicyMismatch},
			{name: "cannot remove deadline", actual: ptr(20), wantReady: false, wantActual: ptr(20), wantReason: sandboxesv1alpha1.ReasonDeadlinePolicyMismatch},
			{name: "zero stored deadline is invalid", desired: ptr(60), actual: ptr(0), wantReady: false, wantActual: ptr(0), wantReason: sandboxesv1alpha1.ReasonDeadlinePolicyMismatch},
		}
		for i, tc := range cases {
			name := fmt.Sprintf("process-policy-deadline-%d-%d", time.Now().UnixNano(), i)
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Annotations: map[string]string{sandboxesv1alpha1.InitialMaxDurationAnnotation: "60"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "alpine:latest"}}, ActiveDeadlineSeconds: tc.actual},
			}
			if tc.actual == nil || *tc.actual != 0 {
				Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			}
			sandbox := &sandboxesv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: "default"}, Spec: sandboxesv1alpha1.SandboxSpec{Image: "alpine:latest", MaxDurationSeconds: tc.desired}}
			r := &SandboxReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
			ready, err := r.reconcilePodDeadline(ctx, sandbox, pod)
			Expect(err).NotTo(HaveOccurred(), tc.name)
			Expect(ready).To(Equal(tc.wantReady), tc.name)
			Expect(pod.Spec.ActiveDeadlineSeconds).To(Equal(tc.wantActual), tc.name)
			Expect(pod.Annotations[sandboxesv1alpha1.InitialMaxDurationAnnotation]).To(Equal("60"), tc.name)
			condition := meta.FindStatusCondition(sandbox.Status.Conditions, sandboxesv1alpha1.ConditionDeadlinePolicyReady)
			Expect(condition).NotTo(BeNil(), tc.name)
			Expect(condition.Reason).To(Equal(tc.wantReason), tc.name)
			if tc.wantReady {
				Expect(condition.Status).To(Equal(metav1.ConditionTrue), tc.name)
			} else {
				Expect(condition.Status).To(Equal(metav1.ConditionFalse), tc.name)
			}
			if tc.actual == nil || *tc.actual != 0 {
				stored := &corev1.Pod{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, stored)).To(Succeed())
				Expect(stored.Spec.ActiveDeadlineSeconds).To(Equal(tc.wantActual), tc.name)
				_ = k8sClient.Delete(ctx, pod)
			}
		}
	})

	It("uses Pod start time for pending and running deadline requeues", func() {
		deadline := int64(120)
		podStart := metav1.NewTime(time.Now().Add(-20 * time.Second))
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "process-policy-deadline-clock", Namespace: "default"},
			Spec: corev1.PodSpec{
				ActiveDeadlineSeconds: &deadline,
				Containers:            []corev1.Container{{Name: "agent", Image: "alpine:latest"}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodPending, StartTime: &podStart},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.Phase = corev1.PodPending
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, pod) }()

		beforeStart := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: pod.Name}, beforeStart)).To(Succeed())
		beforeStart.Status.StartTime = &podStart
		Expect(k8sClient.Status().Update(ctx, beforeStart)).To(Succeed())
		Expect(deadlineRequeueAfter(beforeStart)).To(BeNumerically("~", 100*time.Second, 2*time.Second))

		withoutStart := beforeStart.DeepCopy()
		withoutStart.Status.StartTime = nil
		Expect(deadlineRequeueAfter(withoutStart)).To(Equal(time.Duration(0)))

		sandbox := &sandboxesv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: "default"}, Spec: sandboxesv1alpha1.SandboxSpec{Image: "alpine:latest", MaxDurationSeconds: &deadline}}
		sandbox.Status.StartTime = ptrMetaTime(time.Now().Add(-10 * time.Minute))
		r := &SandboxReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		requeue, err := r.reconcilePod(ctx, sandbox, pod.Name)
		Expect(err).NotTo(HaveOccurred())
		Expect(requeue).To(BeNumerically("~", 100*time.Second, 2*time.Second))
		Expect(sandbox.Status.StartTime.Time).To(BeTemporally("~", time.Now().Add(-10*time.Minute), time.Second))
	})

	It("reconstructs suspended deadline requeue from persisted status after reconciler restart", func() {
		name := fmt.Sprintf("process-policy-suspended-%d", time.Now().UnixNano())
		claimName := name + "-claim"
		deadline := int64(180)
		firstStart := metav1.NewTime(time.Now().Add(-20 * time.Second))
		sandbox := &sandboxesv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: sandboxesv1alpha1.SandboxSpec{
				Image:              "alpine:latest",
				Suspend:            true,
				MaxDurationSeconds: &deadline,
				Workspace:          sandboxesv1alpha1.WorkspaceSpec{PersistentVolumeClaim: &sandboxesv1alpha1.PersistentVolumeClaimWorkspace{ClaimName: claimName}},
			},
		}
		Expect(k8sClient.Create(ctx, testWorkspaceClaim(claimName))).To(Succeed())
		Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
		defer func() {
			stored := &sandboxesv1alpha1.Sandbox{}
			if k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, stored) == nil {
				_ = k8sClient.Delete(ctx, stored)
			}
			_ = k8sClient.Delete(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: claimName}})
			_ = k8sClient.Delete(ctx, &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name + "-netpol"}})
		}()

		stored := &sandboxesv1alpha1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, stored)).To(Succeed())
		stored.Status.Phase = sandboxesv1alpha1.SandboxPhaseRunning
		stored.Status.SuspensionPhase = sandboxesv1alpha1.SuspensionPhaseSuspended
		stored.Status.FirstPodStartTime = &firstStart
		Expect(k8sClient.Status().Update(ctx, stored)).To(Succeed())

		freshReconciler := &SandboxReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		result, err := freshReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically("~", 160*time.Second, 10*time.Second))

		restartedReconciler := &SandboxReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		result, err = restartedReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically("~", 160*time.Second, 10*time.Second))
		pod := &corev1.Pod{}
		podErr := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: name + "-sandbox"}, pod)
		Expect(apierrors.IsNotFound(podErr)).To(BeTrue())
	})

	It("fails suspended sandboxes immediately when the absolute deadline has expired", func() {
		name := fmt.Sprintf("process-policy-expired-suspend-%d", time.Now().UnixNano())
		claimName := name + "-claim"
		deadline := int64(10)
		firstStart := metav1.NewTime(time.Now().Add(-30 * time.Second))
		sandbox := &sandboxesv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: sandboxesv1alpha1.SandboxSpec{
				Image:              "alpine:latest",
				Suspend:            true,
				MaxDurationSeconds: &deadline,
				Workspace:          sandboxesv1alpha1.WorkspaceSpec{PersistentVolumeClaim: &sandboxesv1alpha1.PersistentVolumeClaimWorkspace{ClaimName: claimName}},
			},
		}
		Expect(k8sClient.Create(ctx, testWorkspaceClaim(claimName))).To(Succeed())
		Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
		defer func() {
			stored := &sandboxesv1alpha1.Sandbox{}
			if k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, stored) == nil {
				_ = k8sClient.Delete(ctx, stored)
			}
			_ = k8sClient.Delete(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: claimName}})
		}()
		stored := &sandboxesv1alpha1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, stored)).To(Succeed())
		stored.Status.Phase = sandboxesv1alpha1.SandboxPhaseRunning
		stored.Status.SuspensionPhase = sandboxesv1alpha1.SuspensionPhaseSuspended
		stored.Status.FirstPodStartTime = &firstStart
		Expect(k8sClient.Status().Update(ctx, stored)).To(Succeed())

		r := &SandboxReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(time.Duration(0)))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, stored)).To(Succeed())
		Expect(stored.Status.Phase).To(Equal(sandboxesv1alpha1.SandboxPhaseFailed))
		Expect(stored.Status.CompletionTime).NotTo(BeNil())
		condition := meta.FindStatusCondition(stored.Status.Conditions, sandboxesv1alpha1.ConditionReady)
		Expect(condition).NotTo(BeNil())
		Expect(condition.Reason).To(Equal("DeadlineExceeded"))
		pod := &corev1.Pod{}
		podErr := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: name + "-sandbox"}, pod)
		Expect(apierrors.IsNotFound(podErr)).To(BeTrue())
	})

	It("clamps positive suspended deadline requeues to one millisecond", func() {
		deadline := int64(5)
		sandbox := &sandboxesv1alpha1.Sandbox{
			Spec:   sandboxesv1alpha1.SandboxSpec{MaxDurationSeconds: &deadline},
			Status: sandboxesv1alpha1.SandboxStatus{FirstPodStartTime: ptrMetaTime(time.Now())},
		}
		Expect(suspendedDeadlineRequeueAfter(sandbox, time.Now().Add(5*time.Second-time.Microsecond))).To(Equal(time.Millisecond))
		Expect(suspendedDeadlineRequeueAfter(sandbox, time.Now().Add(5*time.Second))).To(Equal(time.Duration(0)))
		sandbox.Status.FirstPodStartTime = nil
		Expect(suspendedDeadlineRequeueAfter(sandbox, time.Now())).To(Equal(time.Duration(0)))
		sandbox.Status.FirstPodStartTime = ptrMetaTime(time.Now())
		sandbox.Spec.MaxDurationSeconds = nil
		Expect(suspendedDeadlineRequeueAfter(sandbox, time.Now())).To(Equal(time.Duration(0)))
	})

	It("can immediately expire a running Pod when a deadline is set below its age", func() {
		name := fmt.Sprintf("process-policy-expired-%d", time.Now().UnixNano())
		podStart := metav1.NewTime(time.Now().Add(-30 * time.Second))
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Annotations: map[string]string{sandboxesv1alpha1.InitialMaxDurationAnnotation: "0"}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "alpine:latest"}}},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning, StartTime: &podStart},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, StartTime: &podStart}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, pod) }()

		deadline := int64(10)
		sandbox := &sandboxesv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: "default"}, Spec: sandboxesv1alpha1.SandboxSpec{Image: "alpine:latest", MaxDurationSeconds: &deadline}}
		r := &SandboxReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		requeue, err := r.reconcilePod(ctx, sandbox, name)
		Expect(err).NotTo(HaveOccurred())
		Expect(requeue).To(Equal(minimumPositiveRequeue))
		stored := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, stored)).To(Succeed())
		Expect(stored.Spec.ActiveDeadlineSeconds).To(Equal(&deadline))
		Expect(stored.Annotations[sandboxesv1alpha1.InitialMaxDurationAnnotation]).To(Equal("0"))
	})

	It("makes deadline mismatch Ready false and clears it when policy matches", func() {
		actual, desired := int64(30), int64(60)
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "process-policy-deadline-ready", Namespace: "default"},
			Spec:       corev1.PodSpec{ActiveDeadlineSeconds: &actual, Containers: []corev1.Container{{Name: "agent", Image: "alpine:latest"}}},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.Phase = corev1.PodRunning
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, pod) }()
		sandbox := &sandboxesv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: "default"}, Spec: sandboxesv1alpha1.SandboxSpec{Image: "alpine:latest", MaxDurationSeconds: &desired}}
		r := &SandboxReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.reconcilePod(ctx, sandbox, pod.Name)
		Expect(err).NotTo(HaveOccurred())
		ready := meta.FindStatusCondition(sandbox.Status.Conditions, sandboxesv1alpha1.ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(sandboxesv1alpha1.ReasonDeadlinePolicyMismatch))
		firstTransition := ready.LastTransitionTime
		_, err = r.reconcilePod(ctx, sandbox, pod.Name)
		Expect(err).NotTo(HaveOccurred())
		ready = meta.FindStatusCondition(sandbox.Status.Conditions, sandboxesv1alpha1.ConditionReady)
		Expect(ready.LastTransitionTime).To(Equal(firstTransition))

		desired = actual
		_, err = r.reconcilePod(ctx, sandbox, pod.Name)
		Expect(err).NotTo(HaveOccurred())
		ready = meta.FindStatusCondition(sandbox.Status.Conditions, sandboxesv1alpha1.ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		policy := meta.FindStatusCondition(sandbox.Status.Conditions, sandboxesv1alpha1.ConditionDeadlinePolicyReady)
		Expect(policy.Status).To(Equal(metav1.ConditionTrue))
	})

	It("returns Pod deadline patch failures for retry", func() {
		name := fmt.Sprintf("process-policy-patch-error-%d", time.Now().UnixNano())
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "alpine:latest"}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, pod) }()
		deadline := int64(30)
		sandbox := &sandboxesv1alpha1.Sandbox{Spec: sandboxesv1alpha1.SandboxSpec{Image: "alpine:latest", MaxDurationSeconds: &deadline}}
		patchErr := fmt.Errorf("patch rejected")
		r := &SandboxReconciler{Client: failingPatchClient{Client: k8sClient, err: patchErr}, Scheme: k8sClient.Scheme()}
		_, err := r.reconcilePodDeadline(ctx, sandbox, pod)
		Expect(err).To(MatchError(patchErr))
		Expect(meta.FindStatusCondition(sandbox.Status.Conditions, sandboxesv1alpha1.ConditionDeadlinePolicyReady)).To(BeNil())
		stored := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, stored)).To(Succeed())
		Expect(stored.Spec.ActiveDeadlineSeconds).To(BeNil())
	})

	It("preserves terminal Sandbox state while reporting deadline mismatch", func() {
		name := fmt.Sprintf("process-policy-terminal-%d", time.Now().UnixNano())
		deadline := int64(30)
		sandbox := &sandboxesv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       sandboxesv1alpha1.SandboxSpec{Image: "alpine:latest"},
			Status: sandboxesv1alpha1.SandboxStatus{
				Phase:          sandboxesv1alpha1.SandboxPhaseSucceeded,
				PodName:        name + "-sandbox",
				ExitCode:       ptrInt32(0),
				CompletionTime: &metav1.Time{Time: time.Now().Add(-time.Minute)},
				Conditions:     []metav1.Condition{{Type: sandboxesv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "PodCompleted", Message: "finished"}},
			},
		}
		Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
		sandbox.Status = sandboxesv1alpha1.SandboxStatus{
			Phase:          sandboxesv1alpha1.SandboxPhaseSucceeded,
			PodName:        name + "-sandbox",
			ExitCode:       ptrInt32(0),
			CompletionTime: &metav1.Time{Time: time.Now().Add(-time.Minute)},
			Conditions:     []metav1.Condition{{Type: sandboxesv1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "PodCompleted", Message: "finished", LastTransitionTime: metav1.Now()}},
		}
		Expect(k8sClient.Status().Update(ctx, sandbox)).To(Succeed())
		podName := name + "-sandbox"
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: "default"},
			Spec:       corev1.PodSpec{ActiveDeadlineSeconds: &deadline, Containers: []corev1.Container{{Name: "agent", Image: "alpine:latest"}}},
			Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.Phase = corev1.PodSucceeded
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(ctx, sandbox)
			_ = k8sClient.Delete(ctx, pod)
		}()

		r := &SandboxReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: name}})
		Expect(err).NotTo(HaveOccurred())
		updated := &sandboxesv1alpha1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, updated)).To(Succeed())
		Expect(updated.Status.Phase).To(Equal(sandboxesv1alpha1.SandboxPhaseSucceeded))
		Expect(*updated.Status.ExitCode).To(Equal(int32(0)))
		Expect(updated.Status.CompletionTime).To(Equal(sandbox.Status.CompletionTime))
		ready := meta.FindStatusCondition(updated.Status.Conditions, sandboxesv1alpha1.ConditionReady)
		Expect(ready.Reason).To(Equal("PodCompleted"))
		policy := meta.FindStatusCondition(updated.Status.Conditions, sandboxesv1alpha1.ConditionDeadlinePolicyReady)
		Expect(policy.Status).To(Equal(metav1.ConditionFalse))
		storedPod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: pod.Name}, storedPod)).To(Succeed())
		Expect(storedPod.Spec.ActiveDeadlineSeconds).To(Equal(&deadline))
	})

	It("does not recreate children while a Sandbox is being deleted", func() {
		name := fmt.Sprintf("process-policy-deleting-%d", time.Now().UnixNano())
		sandbox := &sandboxesv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Finalizers: []string{"test.finalizer"}},
			Spec:       sandboxesv1alpha1.SandboxSpec{Image: "alpine:latest"},
		}
		Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
		Expect(k8sClient.Delete(ctx, sandbox)).To(Succeed())
		key := types.NamespacedName{Namespace: "default", Name: name}
		Expect(k8sClient.Get(ctx, key, sandbox)).To(Succeed())
		Expect(sandbox.DeletionTimestamp.IsZero()).To(BeFalse())

		r := &SandboxReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		pod := &corev1.Pod{}
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: name + "-sandbox"}, pod)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())

		sandbox.Finalizers = nil
		Expect(k8sClient.Update(ctx, sandbox)).To(Succeed())
	})

	It("validates legacy-safe process policy and monotonic deadline transitions", func() {
		name := fmt.Sprintf("process-policy-schema-%d", time.Now().UnixNano())
		sandbox := &sandboxesv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: sandboxesv1alpha1.SandboxSpec{Image: "alpine:latest"}}
		Expect(k8sClient.Create(ctx, sandbox)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, sandbox) }()

		key := types.NamespacedName{Namespace: "default", Name: name}
		Expect(k8sClient.Get(ctx, key, sandbox)).To(Succeed())
		explicitFalse := false
		sandbox.Spec.Command = []string{"/bin/sh"}
		Expect(k8sClient.Update(ctx, sandbox)).To(Succeed())

		Expect(k8sClient.Get(ctx, key, sandbox)).To(Succeed())
		sandbox.Spec.ShareProcessNamespace = &explicitFalse
		Expect(k8sClient.Update(ctx, sandbox)).To(Succeed())
		Expect(k8sClient.Get(ctx, key, sandbox)).To(Succeed())
		sandbox.Spec.ShareProcessNamespace = nil
		Expect(k8sClient.Update(ctx, sandbox)).To(Succeed())

		Expect(k8sClient.Get(ctx, key, sandbox)).To(Succeed())
		shareTrue := true
		sandbox.Spec.ShareProcessNamespace = &shareTrue
		Expect(k8sClient.Update(ctx, sandbox)).NotTo(Succeed())

		Expect(k8sClient.Get(ctx, key, sandbox)).To(Succeed())
		ten := int64(10)
		sandbox.Spec.MaxDurationSeconds = &ten
		Expect(k8sClient.Update(ctx, sandbox)).To(Succeed())
		Expect(k8sClient.Get(ctx, key, sandbox)).To(Succeed())
		five := int64(5)
		sandbox.Spec.MaxDurationSeconds = &five
		Expect(k8sClient.Update(ctx, sandbox)).To(Succeed())
		Expect(k8sClient.Get(ctx, key, sandbox)).To(Succeed())
		sandbox.Spec.MaxDurationSeconds = &five
		Expect(k8sClient.Update(ctx, sandbox)).To(Succeed())
		Expect(k8sClient.Get(ctx, key, sandbox)).To(Succeed())
		eleven := int64(11)
		sandbox.Spec.MaxDurationSeconds = &eleven
		Expect(k8sClient.Update(ctx, sandbox)).NotTo(Succeed())
		Expect(k8sClient.Get(ctx, key, sandbox)).To(Succeed())
		sandbox.Spec.MaxDurationSeconds = nil
		Expect(k8sClient.Update(ctx, sandbox)).NotTo(Succeed())
		Expect(k8sClient.Get(ctx, key, sandbox)).To(Succeed())
		zero := int64(0)
		sandbox.Spec.MaxDurationSeconds = &zero
		Expect(k8sClient.Update(ctx, sandbox)).NotTo(Succeed())
		Expect(k8sClient.Get(ctx, key, sandbox)).To(Succeed())
		tooLarge := sandboxesv1alpha1.MaxActiveDeadlineSeconds + 1
		sandbox.Spec.MaxDurationSeconds = &tooLarge
		Expect(k8sClient.Update(ctx, sandbox)).NotTo(Succeed())

		trueName := fmt.Sprintf("process-policy-schema-true-%d", time.Now().UnixNano())
		shareTrue = true
		trueSandbox := &sandboxesv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: trueName, Namespace: "default"},
			Spec:       sandboxesv1alpha1.SandboxSpec{Image: "alpine:latest", ShareProcessNamespace: &shareTrue},
		}
		Expect(k8sClient.Create(ctx, trueSandbox)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, trueSandbox) }()
		trueKey := types.NamespacedName{Namespace: "default", Name: trueName}
		Expect(k8sClient.Get(ctx, trueKey, trueSandbox)).To(Succeed())
		falseValue := false
		trueSandbox.Spec.ShareProcessNamespace = &falseValue
		Expect(k8sClient.Update(ctx, trueSandbox)).NotTo(Succeed())

		envName := fmt.Sprintf("process-policy-env-%d", time.Now().UnixNano())
		customEnvSandbox := &sandboxesv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: envName, Namespace: "default"},
			Spec:       sandboxesv1alpha1.SandboxSpec{Image: "alpine:latest", Env: []corev1.EnvVar{{Name: "BUDDY_CUSTOM_VALUE", Value: "allowed"}}},
		}
		Expect(k8sClient.Create(ctx, customEnvSandbox)).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, customEnvSandbox) }()
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: envName}, customEnvSandbox)).To(Succeed())
		customEnvSandbox.Spec.Env = []corev1.EnvVar{{Name: sandboxesv1alpha1.AgentEnvPodName, Value: "spoof"}}
		Expect(k8sClient.Update(ctx, customEnvSandbox)).NotTo(Succeed())
	})
})

func ptrMetaTime(value time.Time) *metav1.Time {
	result := metav1.NewTime(value)
	return &result
}

func ptrInt32(value int32) *int32 {
	return &value
}

type failingPatchClient struct {
	client.Client
	err error
}

func (c failingPatchClient) Patch(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
	return c.err
}
