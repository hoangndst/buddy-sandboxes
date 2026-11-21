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

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
)

const (
	labelSandboxKey   = "sandboxes.buddy.io/sandbox"
	labelManagedByKey = "sandboxes.buddy.io/managed-by"
	managedByValue    = "buddy-sandbox-controller"

	defaultWorkingDir   = "/workspace"
	defaultMountPath    = "/workspace"
	defaultHomeDir      = "/home/sandbox"
	defaultGitInitImage = "alpine/git:latest"

	nonRootUID = int64(1000)
)

// SandboxReconciler reconciles a Sandbox object
type SandboxReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=sandboxes.buddy.io,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sandboxes.buddy.io,resources=sandboxes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sandboxes.buddy.io,resources=sandboxes/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods/status,verbs=get
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=node.k8s.io,resources=runtimeclasses,verbs=get;list;watch

// Reconcile handles the state transitions for a Sandbox resource.
func (r *SandboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var sandbox sandboxesv1alpha1.Sandbox
	if err := r.Get(ctx, req.NamespacedName, &sandbox); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "unable to fetch Sandbox")
		return ctrl.Result{}, err
	}

	// If already in terminal phase, no further reconciliation is required.
	if sandbox.Status.Phase == sandboxesv1alpha1.SandboxPhaseSucceeded ||
		sandbox.Status.Phase == sandboxesv1alpha1.SandboxPhaseFailed {
		return ctrl.Result{}, nil
	}

	originalStatus := sandbox.Status.DeepCopy()

	// Initialize start time and initial conditions if not already set.
	if sandbox.Status.StartTime == nil {
		now := metav1.Now()
		sandbox.Status.StartTime = &now
		sandbox.Status.Phase = sandboxesv1alpha1.SandboxPhasePending
	}

	// 1. Reconcile NetworkPolicy
	netpolName := fmt.Sprintf("%s-netpol", sandbox.Name)
	if err := r.reconcileNetworkPolicy(ctx, &sandbox, netpolName); err != nil {
		logger.Error(err, "failed to reconcile NetworkPolicy")
		return ctrl.Result{}, err
	}
	sandbox.Status.NetworkPolicyName = netpolName
	meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
		Type:               sandboxesv1alpha1.ConditionNetworkConfigured,
		Status:             metav1.ConditionTrue,
		Reason:             "NetworkPolicyCreated",
		Message:            "Egress filtering network policy is active",
		LastTransitionTime: metav1.Now(),
	})

	// 2. Reconcile Workspace Storage (PVC if persistent, else emptyDir)
	if err := r.reconcileStorage(ctx, &sandbox); err != nil {
		logger.Error(err, "failed to reconcile storage")
		if !statusEqual(originalStatus, &sandbox.Status) {
			if statusErr := r.Status().Update(ctx, &sandbox); statusErr != nil {
				return ctrl.Result{}, fmt.Errorf("%w; additionally failed to persist Sandbox status: %v", err, statusErr)
			}
		}
		return ctrl.Result{}, err
	}

	// 3. Reconcile Sandbox Pod
	podName := fmt.Sprintf("%s-sandbox", sandbox.Name)
	requeueDuration, err := r.reconcilePod(ctx, &sandbox, podName)
	if err != nil {
		logger.Error(err, "failed to reconcile Pod")
		return ctrl.Result{}, err
	}
	sandbox.Status.PodName = podName

	// 4. Update status if changes occurred
	if !statusEqual(originalStatus, &sandbox.Status) {
		if err := r.Status().Update(ctx, &sandbox); err != nil {
			logger.Error(err, "failed to update Sandbox status")
			return ctrl.Result{}, err
		}
	}

	if requeueDuration > 0 {
		return ctrl.Result{RequeueAfter: requeueDuration}, nil
	}

	return ctrl.Result{}, nil
}

func (r *SandboxReconciler) reconcileNetworkPolicy(ctx context.Context, sandbox *sandboxesv1alpha1.Sandbox, name string) error {
	var netpol networkingv1.NetworkPolicy
	err := r.Get(ctx, client.ObjectKey{Namespace: sandbox.Namespace, Name: name}, &netpol)
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return err
	}

	netSpec := sandbox.Spec.Network

	egressRules := []networkingv1.NetworkPolicyEgressRule{}

	// DNS rule: allow port 53 (UDP and TCP)
	if netSpec.AllowDNS {
		dnsPortUDP := intstr.FromInt32(53)
		udpProto := corev1.ProtocolUDP
		tcpProto := corev1.ProtocolTCP

		egressRules = append(egressRules, networkingv1.NetworkPolicyEgressRule{
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &udpProto, Port: &dnsPortUDP},
				{Protocol: &tcpProto, Port: &dnsPortUDP},
			},
		})
	}

	// Web egress rule: allow ports 80 and 443 with blocked internal and metadata CIDRs
	if netSpec.AllowInternetWeb {
		port80 := intstr.FromInt32(80)
		port443 := intstr.FromInt32(443)
		tcpProto := corev1.ProtocolTCP

		exceptCIDRs := []string{}
		if netSpec.BlockMetadataService {
			exceptCIDRs = append(exceptCIDRs, "169.254.169.254/32")
		}
		if netSpec.BlockClusterInternal {
			exceptCIDRs = append(exceptCIDRs,
				"10.0.0.0/8",
				"172.16.0.0/12",
				"192.168.0.0/16",
				"100.64.0.0/10",
			)
		}

		egressRules = append(egressRules, networkingv1.NetworkPolicyEgressRule{
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &tcpProto, Port: &port80},
				{Protocol: &tcpProto, Port: &port443},
			},
			To: []networkingv1.NetworkPolicyPeer{
				{
					IPBlock: &networkingv1.IPBlock{
						CIDR:   "0.0.0.0/0",
						Except: exceptCIDRs,
					},
				},
			},
		})
	}

	desiredNetPol := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: sandbox.Namespace,
			Labels: map[string]string{
				labelSandboxKey:   sandbox.Name,
				labelManagedByKey: managedByValue,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					labelSandboxKey: sandbox.Name,
				},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{},
			Egress:  egressRules,
		},
	}

	if err := controllerutil.SetControllerReference(sandbox, desiredNetPol, r.Scheme); err != nil {
		return err
	}

	return r.Create(ctx, desiredNetPol)
}

func (r *SandboxReconciler) reconcileStorage(ctx context.Context, sandbox *sandboxesv1alpha1.Sandbox) error {
	if sandbox.Spec.Workspace.PersistentVolumeClaim != nil && sandbox.Spec.Workspace.PersistentVolumeClaim.ClaimName != "" {
		claimName := sandbox.Spec.Workspace.PersistentVolumeClaim.ClaimName
		var pvc corev1.PersistentVolumeClaim
		err := r.Get(ctx, client.ObjectKey{Namespace: sandbox.Namespace, Name: claimName}, &pvc)
		if err != nil {
			if errors.IsNotFound(err) {
				meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
					Type:               sandboxesv1alpha1.ConditionStorageConfigured,
					Status:             metav1.ConditionFalse,
					Reason:             "PVCNotFound",
					Message:            fmt.Sprintf("PersistentVolumeClaim %q not found in namespace %s", claimName, sandbox.Namespace),
					LastTransitionTime: metav1.Now(),
				})
				return fmt.Errorf("persistentvolumeclaim %q not found: %w", claimName, err)
			}
			return err
		}
		sandbox.Status.PVCName = claimName
		meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
			Type:               sandboxesv1alpha1.ConditionStorageConfigured,
			Status:             metav1.ConditionTrue,
			Reason:             "ExternalPVCAttached",
			Message:            fmt.Sprintf("External PersistentVolumeClaim %q attached", claimName),
			LastTransitionTime: metav1.Now(),
		})
		return nil
	}

	if sandbox.Spec.Workspace.Ephemeral || sandbox.Spec.Workspace.Size == "" {
		meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
			Type:               sandboxesv1alpha1.ConditionStorageConfigured,
			Status:             metav1.ConditionTrue,
			Reason:             "EphemeralEmptyDir",
			Message:            "Using ephemeral emptyDir storage for workspace",
			LastTransitionTime: metav1.Now(),
		})
		return nil
	}

	pvcName := fmt.Sprintf("%s-workspace", sandbox.Name)
	var pvc corev1.PersistentVolumeClaim
	err := r.Get(ctx, client.ObjectKey{Namespace: sandbox.Namespace, Name: pvcName}, &pvc)
	if err == nil {
		sandbox.Status.PVCName = pvcName
		return nil
	}
	if !errors.IsNotFound(err) {
		return err
	}

	quantity, err := resource.ParseQuantity(sandbox.Spec.Workspace.Size)
	if err != nil {
		return fmt.Errorf("invalid workspace size %q: %w", sandbox.Spec.Workspace.Size, err)
	}

	desiredPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: sandbox.Namespace,
			Labels: map[string]string{
				labelSandboxKey:   sandbox.Name,
				labelManagedByKey: managedByValue,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: quantity,
				},
			},
			StorageClassName: sandbox.Spec.Workspace.StorageClassName,
		},
	}

	if err := controllerutil.SetControllerReference(sandbox, desiredPVC, r.Scheme); err != nil {
		return err
	}

	if err := r.Create(ctx, desiredPVC); err != nil {
		return err
	}

	sandbox.Status.PVCName = pvcName
	meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
		Type:               sandboxesv1alpha1.ConditionStorageConfigured,
		Status:             metav1.ConditionTrue,
		Reason:             "PVCCreated",
		Message:            "PersistentVolumeClaim created for workspace",
		LastTransitionTime: metav1.Now(),
	})

	return nil
}

func (r *SandboxReconciler) reconcilePod(ctx context.Context, sandbox *sandboxesv1alpha1.Sandbox, name string) (time.Duration, error) {
	var pod corev1.Pod
	err := r.Get(ctx, client.ObjectKey{Namespace: sandbox.Namespace, Name: name}, &pod)
	if err != nil && !errors.IsNotFound(err) {
		return 0, err
	}

	if errors.IsNotFound(err) {
		// Pre-flight validation of RuntimeClass if specified
		if sandbox.Spec.RuntimeClassName != nil && *sandbox.Spec.RuntimeClassName != "" {
			rcName := *sandbox.Spec.RuntimeClassName
			var rc nodev1.RuntimeClass
			if getErr := r.Get(ctx, client.ObjectKey{Name: rcName}, &rc); getErr != nil {
				if errors.IsNotFound(getErr) {
					sandbox.Status.Phase = sandboxesv1alpha1.SandboxPhasePending
					now := metav1.Now()
					meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
						Type:               sandboxesv1alpha1.ConditionRuntimeClassReady,
						Status:             metav1.ConditionFalse,
						Reason:             sandboxesv1alpha1.ReasonRuntimeClassUnavailable,
						Message:            fmt.Sprintf("RuntimeClass %q not found on cluster", rcName),
						LastTransitionTime: now,
					})
					meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
						Type:               sandboxesv1alpha1.ConditionReady,
						Status:             metav1.ConditionFalse,
						Reason:             sandboxesv1alpha1.ReasonRuntimeClassUnavailable,
						Message:            fmt.Sprintf("RuntimeClass %q not found on cluster", rcName),
						LastTransitionTime: now,
					})
					return 0, nil
				}
				return 0, getErr
			}
			meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
				Type:               sandboxesv1alpha1.ConditionRuntimeClassReady,
				Status:             metav1.ConditionTrue,
				Reason:             sandboxesv1alpha1.ReasonRuntimeClassValid,
				Message:            fmt.Sprintf("RuntimeClass %q found on cluster", rcName),
				LastTransitionTime: metav1.Now(),
			})
		}

		desiredPod := r.buildSandboxPod(sandbox, name)
		if err := controllerutil.SetControllerReference(sandbox, desiredPod, r.Scheme); err != nil {
			return 0, err
		}
		if err := r.Create(ctx, desiredPod); err != nil {
			return 0, err
		}

		sandbox.Status.Phase = sandboxesv1alpha1.SandboxPhasePending
		meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
			Type:               sandboxesv1alpha1.ConditionInitialized,
			Status:             metav1.ConditionTrue,
			Reason:             "PodCreated",
			Message:            "Sandbox pod created and pending scheduling",
			LastTransitionTime: metav1.Now(),
		})
		meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
			Type:               sandboxesv1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             "PodPending",
			Message:            "Pod is waiting to run",
			LastTransitionTime: metav1.Now(),
		})
		return 0, nil
	}

	// Pod already exists, inspect status
	return r.observePodStatus(sandbox, &pod)
}

func (r *SandboxReconciler) buildSandboxPod(sandbox *sandboxesv1alpha1.Sandbox, name string) *corev1.Pod {
	mountPath := sandbox.Spec.Workspace.MountPath
	if mountPath == "" {
		mountPath = defaultMountPath
	}

	workingDir := sandbox.Spec.WorkingDir
	if workingDir == "" {
		workingDir = defaultWorkingDir
	}

	volumes := []corev1.Volume{
		{
			Name: "tmp-volume",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		{
			Name: "home-volume",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
	}

	if sandbox.Spec.Workspace.PersistentVolumeClaim != nil && sandbox.Spec.Workspace.PersistentVolumeClaim.ClaimName != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "workspace-volume",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: sandbox.Spec.Workspace.PersistentVolumeClaim.ClaimName,
					ReadOnly:  sandbox.Spec.Workspace.PersistentVolumeClaim.ReadOnly,
				},
			},
		})
	} else if sandbox.Spec.Workspace.Ephemeral || sandbox.Spec.Workspace.Size == "" {
		volumes = append(volumes, corev1.Volume{
			Name: "workspace-volume",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		})
	} else {
		volumes = append(volumes, corev1.Volume{
			Name: "workspace-volume",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: fmt.Sprintf("%s-workspace", sandbox.Name),
				},
			},
		})
	}

	workspaceReadOnly := false
	if sandbox.Spec.Workspace.PersistentVolumeClaim != nil && sandbox.Spec.Workspace.PersistentVolumeClaim.ReadOnly {
		workspaceReadOnly = true
	}

	volumeMounts := []corev1.VolumeMount{
		{Name: "workspace-volume", MountPath: mountPath, ReadOnly: workspaceReadOnly},
		{Name: "tmp-volume", MountPath: "/tmp"},
		{Name: "home-volume", MountPath: defaultHomeDir},
	}

	envVars := []corev1.EnvVar{
		{Name: "HOME", Value: defaultHomeDir},
	}
	envVars = append(envVars, sandbox.Spec.Env...)

	boolFalse := false
	boolTrue := true
	uid := nonRootUID
	onRootMismatch := corev1.FSGroupChangeOnRootMismatch

	container := corev1.Container{
		Name:            "agent",
		Image:           sandbox.Spec.Image,
		Command:         sandbox.Spec.Command,
		Args:            sandbox.Spec.Args,
		WorkingDir:      workingDir,
		Resources:       sandbox.Spec.Resources,
		Env:             envVars,
		EnvFrom:         sandbox.Spec.EnvFrom,
		VolumeMounts:    volumeMounts,
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: &boolFalse,
			ReadOnlyRootFilesystem:   &boolTrue,
			RunAsNonRoot:             &boolTrue,
			RunAsUser:                &uid,
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
		},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: sandbox.Namespace,
			Labels: map[string]string{
				labelSandboxKey:   sandbox.Name,
				labelManagedByKey: managedByValue,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:        &boolTrue,
				RunAsUser:           &uid,
				FSGroup:             &uid,
				FSGroupChangePolicy: &onRootMismatch,
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			Containers:       []corev1.Container{container},
			Volumes:          volumes,
			RuntimeClassName: sandbox.Spec.RuntimeClassName,
		},
	}

	if sandbox.Spec.Workspace.Git != nil && sandbox.Spec.Workspace.Git.URL != "" {
		pod.Spec.InitContainers = []corev1.Container{r.buildGitInitContainer(sandbox, mountPath)}
	}

	if sandbox.Spec.MaxDurationSeconds != nil {
		pod.Spec.ActiveDeadlineSeconds = sandbox.Spec.MaxDurationSeconds
	}

	return pod
}

const gitCloneScript = `set -e
TARGET="$WORKSPACE_MOUNT"
if [ -n "$GIT_SUBPATH" ]; then
  TARGET="$WORKSPACE_MOUNT/$GIT_SUBPATH"
fi
mkdir -p "$TARGET"
cd "$TARGET"

# The shared emptyDir workspace root may be owned by root with group write access.
# Trust only this configured repository path for the non-root agent container.
git config --global --add safe.directory "$TARGET"

if [ -d .git ]; then
  echo "Git repository already exists in $TARGET"
  exit 0
fi

echo "Cloning repository $GIT_URL into $TARGET..."
if [ -n "$GIT_TOKEN" ] || [ -n "$GIT_PASSWORD" ]; then
  git config --global credential.helper '!f() { echo "username=${GIT_USERNAME:-x-access-token}"; echo "password=${GIT_TOKEN:-$GIT_PASSWORD}"; }; f'
fi

CLONE_ARGS=""
if [ -n "$GIT_DEPTH" ] && [ "$GIT_DEPTH" -gt 0 ]; then
  CLONE_ARGS="$CLONE_ARGS --depth $GIT_DEPTH"
fi

if [ -n "$GIT_REVISION" ]; then
  if git clone $CLONE_ARGS --branch "$GIT_REVISION" "$GIT_URL" . 2>/dev/null; then
    echo "Cloned branch/tag $GIT_REVISION successfully"
    exit 0
  fi
  git init
  git remote add origin "$GIT_URL"
  git fetch $CLONE_ARGS origin "$GIT_REVISION"
  git checkout FETCH_HEAD
else
  git clone $CLONE_ARGS "$GIT_URL" .
fi
`

func (r *SandboxReconciler) buildGitInitContainer(sandbox *sandboxesv1alpha1.Sandbox, mountPath string) corev1.Container {
	boolFalse := false
	boolTrue := true
	uid := nonRootUID

	gitSpec := sandbox.Spec.Workspace.Git
	envVars := []corev1.EnvVar{
		{Name: "HOME", Value: defaultHomeDir},
		{Name: "WORKSPACE_MOUNT", Value: mountPath},
		{Name: "GIT_URL", Value: gitSpec.URL},
		{Name: "GIT_REVISION", Value: gitSpec.Revision},
		{Name: "GIT_SUBPATH", Value: gitSpec.Subpath},
	}
	if gitSpec.Depth > 0 {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "GIT_DEPTH",
			Value: fmt.Sprintf("%d", gitSpec.Depth),
		})
	}

	container := corev1.Container{
		Name:            "git-init",
		Image:           defaultGitInitImage,
		Command:         []string{"/bin/sh", "-c"},
		Args:            []string{gitCloneScript},
		Env:             envVars,
		ImagePullPolicy: corev1.PullIfNotPresent,
		VolumeMounts: []corev1.VolumeMount{
			{Name: "workspace-volume", MountPath: mountPath},
			{Name: "tmp-volume", MountPath: "/tmp"},
			{Name: "home-volume", MountPath: defaultHomeDir},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: &boolFalse,
			ReadOnlyRootFilesystem:   &boolFalse,
			RunAsNonRoot:             &boolTrue,
			RunAsUser:                &uid,
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
		},
	}

	if gitSpec.SecretRef != nil && gitSpec.SecretRef.Name != "" {
		container.EnvFrom = []corev1.EnvFromSource{
			{
				SecretRef: &corev1.SecretEnvSource{
					LocalObjectReference: *gitSpec.SecretRef,
				},
			},
		}
	}

	return container
}

func checkPendingPodErrors(pod *corev1.Pod, now metav1.Time, status *sandboxesv1alpha1.SandboxStatus) bool {
	// Check for init container waiting errors or failures
	for _, ics := range pod.Status.InitContainerStatuses {
		if ics.State.Waiting != nil {
			reason := ics.State.Waiting.Reason
			if reason == "ImagePullBackOff" || reason == "ErrImagePull" || reason == "CrashLoopBackOff" {
				status.Phase = sandboxesv1alpha1.SandboxPhaseFailed
				status.CompletionTime = &now
				meta.SetStatusCondition(&status.Conditions, metav1.Condition{
					Type:               sandboxesv1alpha1.ConditionReady,
					Status:             metav1.ConditionFalse,
					Reason:             reason,
					Message:            fmt.Sprintf("Init container %s waiting: %s", ics.Name, ics.State.Waiting.Message),
					LastTransitionTime: now,
				})
				return true
			}
		}
		if ics.State.Terminated != nil && ics.State.Terminated.ExitCode != 0 {
			status.Phase = sandboxesv1alpha1.SandboxPhaseFailed
			status.CompletionTime = &now
			meta.SetStatusCondition(&status.Conditions, metav1.Condition{
				Type:               sandboxesv1alpha1.ConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             "InitContainerFailed",
				Message:            fmt.Sprintf("Init container %s terminated with exit code %d", ics.Name, ics.State.Terminated.ExitCode),
				LastTransitionTime: now,
			})
			return true
		}
	}

	// Check for container waiting errors (e.g. ImagePullBackOff, ErrImagePull)
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil {
			reason := cs.State.Waiting.Reason
			if reason == "ImagePullBackOff" || reason == "ErrImagePull" || reason == "CrashLoopBackOff" {
				status.Phase = sandboxesv1alpha1.SandboxPhaseFailed
				status.CompletionTime = &now
				meta.SetStatusCondition(&status.Conditions, metav1.Condition{
					Type:               sandboxesv1alpha1.ConditionReady,
					Status:             metav1.ConditionFalse,
					Reason:             reason,
					Message:            cs.State.Waiting.Message,
					LastTransitionTime: now,
				})
				return true
			}
		}
	}

	// Check for scheduler conditions (e.g. Unschedulable)
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse && cond.Reason == "Unschedulable" {
			status.Phase = sandboxesv1alpha1.SandboxPhasePending
			meta.SetStatusCondition(&status.Conditions, metav1.Condition{
				Type:               sandboxesv1alpha1.ConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             cond.Reason,
				Message:            cond.Message,
				LastTransitionTime: now,
			})
			return true
		}
	}

	return false
}

func extractAgentExitCode(pod *corev1.Pod) *int32 {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == "agent" && cs.State.Terminated != nil {
			return &cs.State.Terminated.ExitCode
		}
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil {
			return &cs.State.Terminated.ExitCode
		}
	}
	return nil
}

func (r *SandboxReconciler) observePodStatus(sandbox *sandboxesv1alpha1.Sandbox, pod *corev1.Pod) (time.Duration, error) {
	now := metav1.Now()

	switch pod.Status.Phase {
	case corev1.PodPending:
		if checkPendingPodErrors(pod, now, &sandbox.Status) {
			return 0, nil
		}

		sandbox.Status.Phase = sandboxesv1alpha1.SandboxPhasePending
		meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
			Type:               sandboxesv1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             "PodPending",
			Message:            "Sandbox container is initializing or waiting for image pull",
			LastTransitionTime: now,
		})
		return 0, nil

	case corev1.PodRunning:
		sandbox.Status.Phase = sandboxesv1alpha1.SandboxPhaseRunning
		meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
			Type:               sandboxesv1alpha1.ConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             "PodRunning",
			Message:            "Sandbox pod is active and running",
			LastTransitionTime: now,
		})

		// Calculate remaining duration if deadline configured
		if sandbox.Spec.MaxDurationSeconds != nil && sandbox.Status.StartTime != nil {
			elapsed := time.Since(sandbox.Status.StartTime.Time)
			totalLimit := time.Duration(*sandbox.Spec.MaxDurationSeconds) * time.Second
			if elapsed < totalLimit {
				return totalLimit - elapsed, nil
			}
		}
		return 0, nil

	case corev1.PodSucceeded:
		sandbox.Status.Phase = sandboxesv1alpha1.SandboxPhaseSucceeded
		zero := int32(0)
		sandbox.Status.ExitCode = &zero
		if sandbox.Status.CompletionTime == nil {
			sandbox.Status.CompletionTime = &now
		}
		meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
			Type:               sandboxesv1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             "PodCompleted",
			Message:            "Sandbox process finished successfully with exit code 0",
			LastTransitionTime: now,
		})
		return 0, nil

	case corev1.PodFailed:
		sandbox.Status.Phase = sandboxesv1alpha1.SandboxPhaseFailed
		if sandbox.Status.CompletionTime == nil {
			sandbox.Status.CompletionTime = &now
		}

		sandbox.Status.ExitCode = extractAgentExitCode(pod)

		meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
			Type:               sandboxesv1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             pod.Status.Reason,
			Message:            pod.Status.Message,
			LastTransitionTime: now,
		})
		return 0, nil
	}

	return 0, nil
}

func statusEqual(a, b *sandboxesv1alpha1.SandboxStatus) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Phase != b.Phase ||
		a.PodName != b.PodName ||
		a.NetworkPolicyName != b.NetworkPolicyName ||
		a.PVCName != b.PVCName {
		return false
	}
	if (a.ExitCode == nil) != (b.ExitCode == nil) {
		return false
	}
	if a.ExitCode != nil && *a.ExitCode != *b.ExitCode {
		return false
	}
	if len(a.Conditions) != len(b.Conditions) {
		return false
	}
	for i := range a.Conditions {
		if a.Conditions[i].Type != b.Conditions[i].Type ||
			a.Conditions[i].Status != b.Conditions[i].Status ||
			a.Conditions[i].Reason != b.Conditions[i].Reason ||
			a.Conditions[i].Message != b.Conditions[i].Message {
			return false
		}
	}
	return true
}

func (r *SandboxReconciler) findSandboxesForRuntimeClass(ctx context.Context, obj client.Object) []ctrl.Request {
	rc, ok := obj.(*nodev1.RuntimeClass)
	if !ok {
		return nil
	}
	var sandboxes sandboxesv1alpha1.SandboxList
	if err := r.List(ctx, &sandboxes); err != nil {
		return nil
	}
	var reqs []ctrl.Request
	for _, sbx := range sandboxes.Items {
		if sbx.Spec.RuntimeClassName != nil && *sbx.Spec.RuntimeClassName == rc.Name {
			reqs = append(reqs, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Namespace: sbx.Namespace,
					Name:      sbx.Name,
				},
			})
		}
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *SandboxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sandboxesv1alpha1.Sandbox{}).
		Owns(&corev1.Pod{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Watches(&nodev1.RuntimeClass{}, handler.EnqueueRequestsFromMapFunc(r.findSandboxesForRuntimeClass)).
		Named("sandbox").
		Complete(r)
}
