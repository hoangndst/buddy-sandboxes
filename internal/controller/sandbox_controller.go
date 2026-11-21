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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
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
	"github.com/hoangndst/buddy-sandboxes/internal/egressgateway"
)

const (
	labelSandboxKey   = "sandboxes.buddy.io/sandbox"
	labelManagedByKey = "sandboxes.buddy.io/managed-by"
	managedByValue    = "buddy-sandbox-controller"

	defaultWorkingDir      = "/workspace"
	defaultMountPath       = "/workspace"
	defaultHomeDir         = "/home/sandbox"
	defaultGitInitImage    = "alpine/git:latest"
	minimumDeadlineRequeue = time.Second
	minimumPositiveRequeue = time.Millisecond
	reasonErrImagePull     = "ErrImagePull"
	reasonCrashLoopBackOff = "CrashLoopBackOff"

	nonRootUID = int64(1000)
)

// SandboxReconciler reconciles a Sandbox object
type SandboxReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	HMACSecret []byte
}

func (r *SandboxReconciler) getHMACSecret() ([]byte, error) {
	if len(r.HMACSecret) > 0 {
		return r.HMACSecret, nil
	}
	if envSecret := os.Getenv("EGRESS_GATEWAY_HMAC_SECRET"); envSecret != "" && !strings.Contains(envSecret, "REPLACE_WITH_HMAC_SECRET") {
		return []byte(envSecret), nil
	}
	return nil, fmt.Errorf("EGRESS_GATEWAY_HMAC_SECRET is required and must be configured with a private signing key")
}

func (r *SandboxReconciler) proxyEnvVars(sandbox *sandboxesv1alpha1.Sandbox) []corev1.EnvVar {
	if len(sandbox.Spec.Network.AllowedDomains) == 0 {
		return nil
	}

	secret, err := r.getHMACSecret()
	if err != nil {
		return nil
	}

	exp := time.Now().Add(7 * 24 * time.Hour).Unix()
	if sandbox.Spec.MaxDurationSeconds != nil && *sandbox.Spec.MaxDurationSeconds > 0 {
		exp = time.Now().Add(time.Duration(*sandbox.Spec.MaxDurationSeconds)*time.Second + time.Hour).Unix()
	}

	claims := egressgateway.Claims{
		Sub:     sandbox.Name,
		Ns:      sandbox.Namespace,
		Domains: sandbox.Spec.Network.AllowedDomains,
		Exp:     exp,
	}

	token, err := egressgateway.GenerateToken(claims, secret)
	if err != nil {
		return nil
	}

	proxyURL := fmt.Sprintf("http://%s:%s@buddy-sandboxes-egress-gateway.buddy-sandboxes-system.svc.cluster.local:8080", sandbox.Name, token)
	noProxy := "localhost,127.0.0.1,::1,.cluster.local,.svc"

	return []corev1.EnvVar{
		{Name: "HTTP_PROXY", Value: proxyURL},
		{Name: "HTTPS_PROXY", Value: proxyURL},
		{Name: "http_proxy", Value: proxyURL},
		{Name: "https_proxy", Value: proxyURL},
		{Name: "NO_PROXY", Value: noProxy},
		{Name: "no_proxy", Value: noProxy},
	}
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
	if !sandbox.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// If already in terminal phase, no further reconciliation is required.
	if sandbox.Status.Phase == sandboxesv1alpha1.SandboxPhaseSucceeded ||
		sandbox.Status.Phase == sandboxesv1alpha1.SandboxPhaseFailed {
		return r.reconcileTerminal(ctx, &sandbox)
	}

	originalStatus := sandbox.Status.DeepCopy()
	if deadline, ok := absoluteSandboxDeadline(&sandbox); ok && !time.Now().Before(deadline) {
		if err := r.failSandboxDeadline(ctx, &sandbox); err != nil {
			return ctrl.Result{}, err
		}
		if !statusEqual(originalStatus, &sandbox.Status) {
			if err := r.Status().Update(ctx, &sandbox); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

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

	// 3. Apply suspension intent before normal Pod creation.
	podName := fmt.Sprintf("%s-sandbox", sandbox.Name)
	deferToReconcilePod, suspensionRequeue, err := r.reconcileSuspension(ctx, &sandbox, podName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if deferToReconcilePod {
		if !statusEqual(originalStatus, &sandbox.Status) {
			if err := r.Status().Update(ctx, &sandbox); err != nil {
				return ctrl.Result{}, err
			}
		}
		if suspensionRequeue > 0 {
			return ctrl.Result{RequeueAfter: suspensionRequeue}, nil
		}
		return ctrl.Result{}, nil
	}

	// 4. Reconcile Sandbox Pod
	requeueDuration, err := r.reconcilePod(ctx, &sandbox, podName)
	if err != nil {
		logger.Error(err, "failed to reconcile Pod")
		return ctrl.Result{}, err
	}
	sandbox.Status.PodName = podName
	if suspensionRequeue > 0 && (requeueDuration == 0 || suspensionRequeue < requeueDuration) {
		requeueDuration = suspensionRequeue
	}

	// 4. Update status if changes occurred
	if !statusEqual(originalStatus, &sandbox.Status) {
		if err := r.Status().Update(ctx, &sandbox); err != nil {
			logger.Error(err, "failed to update Sandbox status")
			return ctrl.Result{}, err
		}
		r.cleanupFailedInitPod(ctx, &sandbox, podName)
	}

	if requeueDuration > 0 {
		return ctrl.Result{RequeueAfter: requeueDuration}, nil
	}

	return ctrl.Result{}, nil
}

func workspaceSupportsSuspension(sandbox *sandboxesv1alpha1.Sandbox) bool {
	workspace := sandbox.Spec.Workspace
	if pvc := workspace.PersistentVolumeClaim; pvc != nil && pvc.ClaimName != "" {
		return !pvc.ReadOnly
	}
	return !workspaceIsEphemeral(workspace) && workspace.Size != ""
}

func workspaceIsEphemeral(workspace sandboxesv1alpha1.WorkspaceSpec) bool {
	return workspace.Ephemeral == nil || *workspace.Ephemeral
}

func setSuspensionCondition(sandbox *sandboxesv1alpha1.Sandbox, ready bool, reason, message string) {
	conditionStatus := metav1.ConditionFalse
	if ready {
		conditionStatus = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
		Type:               sandboxesv1alpha1.ConditionSuspensionReady,
		Status:             conditionStatus,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	})
}

// reconcileSuspension returns handled=true when normal Pod reconciliation must not run.
func (r *SandboxReconciler) reconcileSuspension(
	ctx context.Context,
	sandbox *sandboxesv1alpha1.Sandbox,
	podName string,
) (handled bool, requeueAfter time.Duration, err error) {
	var pod corev1.Pod
	getErr := r.Get(ctx, client.ObjectKey{Namespace: sandbox.Namespace, Name: podName}, &pod)
	if getErr != nil && !errors.IsNotFound(getErr) {
		return false, 0, getErr
	}
	podExists := getErr == nil

	if sandbox.Spec.Suspend {
		if !workspaceSupportsSuspension(sandbox) {
			setSuspensionCondition(sandbox, false, "WorkspaceNotPersistent", "Suspension requires a writable PVC-backed workspace")
			return false, 0, nil
		}
		// Let normal Pod observation record a terminal result before deleting anything.
		if podExists && pod.DeletionTimestamp == nil && isTerminalPod(&pod) {
			return false, 0, nil
		}

		if sandbox.Status.SuspensionPhase != sandboxesv1alpha1.SuspensionPhaseSuspended {
			sandbox.Status.SuspensionPhase = sandboxesv1alpha1.SuspensionPhaseSuspending
			setSuspensionCondition(sandbox, false, "Suspending", "Waiting for the execution Pod to stop")
		}
		if !podExists {
			sandbox.Status.PodName = ""
			sandbox.Status.PodUID = ""
			sandbox.Status.SuspensionPhase = sandboxesv1alpha1.SuspensionPhaseSuspended
			setSuspensionCondition(sandbox, true, "Suspended", "The execution Pod has stopped and the PVC workspace is retained")
			return true, suspendedDeadlineRequeueAfter(sandbox, time.Now()), nil
		}
		if pod.DeletionTimestamp == nil {
			gracePeriod := int64(10)
			if err := r.Delete(ctx, &pod, &client.DeleteOptions{GracePeriodSeconds: &gracePeriod}); err != nil && !errors.IsNotFound(err) {
				return false, 0, err
			}
		}
		return true, minPositiveDuration(time.Second, suspendedDeadlineRequeueAfter(sandbox, time.Now())), nil
	}

	phase := sandbox.Status.SuspensionPhase
	if phase == sandboxesv1alpha1.SuspensionPhaseSuspending || phase == sandboxesv1alpha1.SuspensionPhaseSuspended {
		if phase == sandboxesv1alpha1.SuspensionPhaseSuspending && podExists && pod.DeletionTimestamp == nil {
			// The suspend request was reversed before deletion began.
			sandbox.Status.SuspensionPhase = sandboxesv1alpha1.SuspensionPhaseNone
			setSuspensionCondition(sandbox, true, "Active", "Sandbox is active")
			return false, 0, nil
		}
		sandbox.Status.SuspensionPhase = sandboxesv1alpha1.SuspensionPhaseResuming
		setSuspensionCondition(sandbox, false, "Resuming", "Waiting for a replacement execution Pod")
		if podExists && pod.DeletionTimestamp != nil {
			return true, minPositiveDuration(time.Second, suspendedDeadlineRequeueAfter(sandbox, time.Now())), nil
		}
		if !podExists {
			sandbox.Status.PodName = ""
			sandbox.Status.PodUID = ""
		}
	}

	if sandbox.Status.SuspensionPhase == "" {
		sandbox.Status.SuspensionPhase = sandboxesv1alpha1.SuspensionPhaseNone
	}
	if sandbox.Status.SuspensionPhase == sandboxesv1alpha1.SuspensionPhaseNone {
		setSuspensionCondition(sandbox, true, "Active", "Sandbox is active")
	}
	return false, 0, nil
}

func absoluteSandboxDeadline(sandbox *sandboxesv1alpha1.Sandbox) (time.Time, bool) {
	if sandbox.Spec.MaxDurationSeconds == nil || *sandbox.Spec.MaxDurationSeconds <= 0 || sandbox.Status.FirstPodStartTime == nil {
		return time.Time{}, false
	}
	return sandbox.Status.FirstPodStartTime.Time.Add(time.Duration(*sandbox.Spec.MaxDurationSeconds) * time.Second), true
}

func suspendedDeadlineRequeueAfter(sandbox *sandboxesv1alpha1.Sandbox, now time.Time) time.Duration {
	deadline, ok := absoluteSandboxDeadline(sandbox)
	if !ok {
		return 0
	}
	remaining := deadline.Sub(now)
	if remaining <= 0 {
		return 0
	}
	if remaining < minimumPositiveRequeue {
		return minimumPositiveRequeue
	}
	return remaining
}

func minPositiveDuration(a, b time.Duration) time.Duration {
	if b <= 0 || a < b {
		return a
	}
	return b
}

func (r *SandboxReconciler) failSandboxDeadline(ctx context.Context, sandbox *sandboxesv1alpha1.Sandbox) error {
	now := metav1.Now()
	r.markSandboxFailed(sandbox, now, nil, "DeadlineExceeded", "Sandbox execution deadline exceeded")
	name := sandbox.Status.PodName
	if name == "" {
		name = sandbox.Name + "-sandbox"
	}
	var pod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Namespace: sandbox.Namespace, Name: name}, &pod); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !isTerminalPod(&pod) && pod.DeletionTimestamp == nil {
		gracePeriod := int64(10)
		if err := r.Delete(ctx, &pod, &client.DeleteOptions{GracePeriodSeconds: &gracePeriod}); err != nil && !errors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *SandboxReconciler) reconcileTerminal(ctx context.Context, sandbox *sandboxesv1alpha1.Sandbox) (ctrl.Result, error) {
	originalStatus := sandbox.Status.DeepCopy()
	if sandbox.Spec.Suspend || (sandbox.Status.SuspensionPhase != "" && sandbox.Status.SuspensionPhase != sandboxesv1alpha1.SuspensionPhaseNone) {
		setSuspensionCondition(sandbox, false, "TerminalSandbox", "Terminal Sandboxes cannot be suspended or resumed")
	}
	podName := sandbox.Status.PodName
	if podName == "" {
		podName = fmt.Sprintf("%s-sandbox", sandbox.Name)
	}
	var pod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Namespace: sandbox.Namespace, Name: podName}, &pod); err != nil {
		if !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	} else {
		if sandbox.Status.Phase == sandboxesv1alpha1.SandboxPhaseFailed && sandbox.Status.ExitCode == nil && !isTerminalPod(&pod) && pod.DeletionTimestamp == nil {
			if err := r.Delete(ctx, &pod); err != nil && !errors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
		if deadlinePoliciesMatch(sandbox, &pod) {
			setDeadlinePolicyCondition(sandbox, sandboxesv1alpha1.ReasonDeadlinePolicyMatched, true)
		} else {
			setDeadlinePolicyCondition(sandbox, sandboxesv1alpha1.ReasonDeadlinePolicyMismatch, false)
		}
	}
	if !statusEqual(originalStatus, &sandbox.Status) {
		if err := r.Status().Update(ctx, sandbox); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *SandboxReconciler) cleanupFailedInitPod(ctx context.Context, sandbox *sandboxesv1alpha1.Sandbox, podName string) {
	if sandbox.Status.Phase != sandboxesv1alpha1.SandboxPhaseFailed || sandbox.Status.ExitCode != nil {
		return
	}
	var pod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Namespace: sandbox.Namespace, Name: podName}, &pod); err == nil {
		if !isTerminalPod(&pod) && pod.DeletionTimestamp == nil {
			_ = r.Delete(ctx, &pod)
		}
	}
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
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
					"kubernetes.io/metadata.name": "kube-system",
				}},
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
					"k8s-app": "kube-dns",
				}},
			}},
		})
	}

	// Web egress rule:
	// If AllowedDomains is configured, egress is routed through buddy-egress-gateway (port 8080)
	// and direct internet egress (0.0.0.0/0) is suppressed.
	// Otherwise, if AllowInternetWeb is true, direct internet egress is permitted.
	if len(netSpec.AllowedDomains) > 0 {
		gwPort := intstr.FromInt32(8080)
		tcpProto := corev1.ProtocolTCP
		egressRules = append(egressRules, networkingv1.NetworkPolicyEgressRule{
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &tcpProto, Port: &gwPort},
			},
			To: []networkingv1.NetworkPolicyPeer{
				{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"kubernetes.io/metadata.name": "buddy-sandboxes-system",
						},
					},
					PodSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app.kubernetes.io/name": "buddy-egress-gateway",
						},
					},
				},
				{
					PodSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app.kubernetes.io/name": "buddy-egress-gateway",
						},
					},
				},
			},
		})
	} else if netSpec.AllowInternetWeb {
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

	if workspaceIsEphemeral(sandbox.Spec.Workspace) || sandbox.Spec.Workspace.Size == "" {
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

		if len(sandbox.Spec.Network.AllowedDomains) > 0 {
			if _, err := r.getHMACSecret(); err != nil {
				return 0, fmt.Errorf("egress gateway configuration error: %w", err)
			}
		}

		desiredPod := r.buildSandboxPod(sandbox, name)
		deadlineSeconds, hasDeadline, valid := newPodDeadlineSeconds(sandbox, time.Now())
		if !valid {
			r.markSandboxFailed(sandbox, metav1.Now(), nil, "DeadlineExceeded", "Less than one whole second remains in the Sandbox execution deadline")
			return 0, nil
		}
		if hasDeadline {
			desiredPod.Spec.ActiveDeadlineSeconds = &deadlineSeconds
			desiredPod.Annotations[sandboxesv1alpha1.PodDeadlineSecondsAnnotation] = strconv.FormatInt(deadlineSeconds, 10)
		} else {
			desiredPod.Spec.ActiveDeadlineSeconds = nil
		}
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
		meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
			Type:               sandboxesv1alpha1.ConditionDeadlinePolicyReady,
			Status:             metav1.ConditionTrue,
			Reason:             sandboxesv1alpha1.ReasonDeadlinePolicyMatched,
			Message:            "Sandbox deadline matches the created Pod",
			LastTransitionTime: metav1.Now(),
		})
		return 0, nil
	}

	// Existing Pods are checked against their own generation's expected deadline.
	policyReady, err := r.reconcilePodDeadline(ctx, sandbox, &pod)
	if err != nil {
		return 0, err
	}
	r.observePodStatus(sandbox, &pod)
	requeueAfter := time.Duration(0)
	sandboxTerminal := sandbox.Status.Phase == sandboxesv1alpha1.SandboxPhaseSucceeded || sandbox.Status.Phase == sandboxesv1alpha1.SandboxPhaseFailed
	if !isTerminalPod(&pod) && !sandboxTerminal {
		if !policyReady {
			condition := meta.FindStatusCondition(sandbox.Status.Conditions, sandboxesv1alpha1.ConditionDeadlinePolicyReady)
			message := "Pod deadline cannot be relaxed or removed"
			if condition != nil {
				message = condition.Message
			}
			meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
				Type:               sandboxesv1alpha1.ConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             sandboxesv1alpha1.ReasonDeadlinePolicyMismatch,
				Message:            message,
				LastTransitionTime: metav1.Now(),
			})
		} else if requeueAfter == 0 && (pod.Status.Phase == corev1.PodPending || pod.Status.Phase == corev1.PodRunning) {
			requeueAfter = podDeadlineRequeueAfter(sandbox, &pod, time.Now())
		}
	}
	return requeueAfter, nil
}

func isTerminalPod(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}

func setDeadlinePolicyCondition(sandbox *sandboxesv1alpha1.Sandbox, reason string, matched bool) {
	status := metav1.ConditionFalse
	message := "Sandbox and Pod deadline policies differ and the Pod deadline cannot be relaxed or removed"
	if matched {
		status = metav1.ConditionTrue
		message = "Sandbox deadline matches the Pod deadline"
	}
	meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
		Type:               sandboxesv1alpha1.ConditionDeadlinePolicyReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	})
}

func deadlinePoliciesMatch(sandbox *sandboxesv1alpha1.Sandbox, pod *corev1.Pod) bool {
	actual := pod.Spec.ActiveDeadlineSeconds
	if actual != nil && *actual <= 0 {
		return false
	}
	if raw, ok := pod.Annotations[sandboxesv1alpha1.PodDeadlineSecondsAnnotation]; ok {
		expected, err := strconv.ParseInt(raw, 10, 64)
		return err == nil && expected > 0 && actual != nil && *actual == expected
	}
	if sandbox.Spec.MaxDurationSeconds == nil {
		return actual == nil
	}
	return actual != nil && *actual == *sandbox.Spec.MaxDurationSeconds
}

func (r *SandboxReconciler) reconcilePodDeadline(ctx context.Context, sandbox *sandboxesv1alpha1.Sandbox, pod *corev1.Pod) (bool, error) {
	actual := pod.Spec.ActiveDeadlineSeconds
	if actual != nil && *actual <= 0 {
		setDeadlinePolicyCondition(sandbox, sandboxesv1alpha1.ReasonDeadlinePolicyMismatch, false)
		return false, nil
	}

	expected, hasAnnotation := podGenerationExpectedDeadline(pod)
	if !hasAnnotation {
		if sandbox.Spec.MaxDurationSeconds == nil && actual == nil {
			setDeadlinePolicyCondition(sandbox, sandboxesv1alpha1.ReasonDeadlinePolicyMatched, true)
			return true, nil
		}
		if sandbox.Spec.MaxDurationSeconds == nil {
			setDeadlinePolicyCondition(sandbox, sandboxesv1alpha1.ReasonDeadlinePolicyMismatch, false)
			return false, nil
		}
		expected = *sandbox.Spec.MaxDurationSeconds
	}
	if expected <= 0 {
		setDeadlinePolicyCondition(sandbox, sandboxesv1alpha1.ReasonDeadlinePolicyMismatch, false)
		return false, nil
	}

	target := expected
	if deadline, ok := absoluteSandboxDeadline(sandbox); ok {
		basis := time.Now()
		if pod.Status.StartTime != nil {
			basis = pod.Status.StartTime.Time
		} else if !pod.CreationTimestamp.IsZero() {
			basis = pod.CreationTimestamp.Time
		}
		remaining := int64(deadline.Sub(basis) / time.Second)
		if remaining < 1 {
			remaining = 1
		}
		if remaining < target {
			target = remaining
		}
	}
	if sandbox.Spec.MaxDurationSeconds != nil && sandbox.Status.FirstPodStartTime == nil && *sandbox.Spec.MaxDurationSeconds < target {
		target = *sandbox.Spec.MaxDurationSeconds
	}
	if actual != nil && target == *actual {
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		if pod.Annotations[sandboxesv1alpha1.PodDeadlineSecondsAnnotation] != strconv.FormatInt(target, 10) {
			base := pod.DeepCopy()
			pod.Annotations[sandboxesv1alpha1.PodDeadlineSecondsAnnotation] = strconv.FormatInt(target, 10)
			if err := r.Patch(ctx, pod, client.MergeFrom(base)); err != nil {
				return false, err
			}
		}
		setDeadlinePolicyCondition(sandbox, sandboxesv1alpha1.ReasonDeadlinePolicyMatched, true)
		return true, nil
	}
	if (actual != nil && target > *actual) || isTerminalPod(pod) {
		setDeadlinePolicyCondition(sandbox, sandboxesv1alpha1.ReasonDeadlinePolicyMismatch, false)
		return false, nil
	}

	base := pod.DeepCopy()
	value := target
	pod.Spec.ActiveDeadlineSeconds = &value
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[sandboxesv1alpha1.PodDeadlineSecondsAnnotation] = strconv.FormatInt(target, 10)
	if err := r.Patch(ctx, pod, client.MergeFrom(base)); err != nil {
		return false, err
	}
	setDeadlinePolicyCondition(sandbox, sandboxesv1alpha1.ReasonDeadlinePolicyPatched, true)
	return true, nil
}

func podGenerationExpectedDeadline(pod *corev1.Pod) (int64, bool) {
	if raw, ok := pod.Annotations[sandboxesv1alpha1.PodDeadlineSecondsAnnotation]; ok {
		value, err := strconv.ParseInt(raw, 10, 64)
		return value, err == nil && value > 0
	}
	return 0, false
}

func newPodDeadlineSeconds(sandbox *sandboxesv1alpha1.Sandbox, now time.Time) (int64, bool, bool) {
	if sandbox.Spec.MaxDurationSeconds == nil {
		return 0, false, true
	}
	if sandbox.Status.FirstPodStartTime == nil {
		return *sandbox.Spec.MaxDurationSeconds, true, true
	}
	deadline, ok := absoluteSandboxDeadline(sandbox)
	if !ok {
		return 0, false, true
	}
	remaining := int64(deadline.Sub(now) / time.Second)
	if remaining < 1 {
		return 0, true, false
	}
	return remaining, true, true
}

func deadlineRequeueAfter(pod *corev1.Pod) time.Duration {
	if pod.Status.StartTime == nil || pod.Spec.ActiveDeadlineSeconds == nil || *pod.Spec.ActiveDeadlineSeconds <= 0 {
		return 0
	}
	const maxDurationSeconds = (1<<63 - 1) / 1_000_000_000
	if *pod.Spec.ActiveDeadlineSeconds > maxDurationSeconds {
		return 0
	}
	expiresAt := pod.Status.StartTime.Time.Add(time.Duration(*pod.Spec.ActiveDeadlineSeconds) * time.Second)
	remaining := time.Until(expiresAt)
	if remaining <= 0 {
		return minimumDeadlineRequeue
	}
	return remaining
}

func podDeadlineRequeueAfter(sandbox *sandboxesv1alpha1.Sandbox, pod *corev1.Pod, now time.Time) time.Duration {
	podLocal := deadlineRequeueAfter(pod)
	absolute, hasAbsolute := absoluteSandboxDeadline(sandbox)
	if !hasAbsolute {
		return podLocal
	}
	remaining := absolute.Sub(now)
	if remaining <= 0 {
		return minimumPositiveRequeue
	}
	absRequeue := maxDuration(remaining, minimumPositiveRequeue)
	if podLocal <= 0 || absRequeue < podLocal {
		return absRequeue
	}
	return podLocal
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
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
	} else if workspaceIsEphemeral(sandbox.Spec.Workspace) || sandbox.Spec.Workspace.Size == "" {
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

	if len(sandbox.Spec.MCPServers) > 0 {
		for _, server := range sandbox.Spec.MCPServers {
			volumes = append(volumes, corev1.Volume{
				Name: mcpScratchVolumeName(server.Name),
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			})
		}
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
	envVars = append(envVars, downwardAPIEnvVars()...)
	if len(sandbox.Spec.MCPServers) > 0 {
		envVars = append(envVars, mcpDiscoveryEnvVars(sandbox.Spec.MCPServers)...)
	}
	if len(sandbox.Spec.Network.AllowedDomains) > 0 {
		envVars = append(envVars, r.proxyEnvVars(sandbox)...)
	}

	boolFalse := false
	boolTrue := true
	shareProcessNamespace := sandbox.Spec.ShareProcessNamespace != nil && *sandbox.Spec.ShareProcessNamespace
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
			Annotations: map[string]string{
				sandboxesv1alpha1.InitialMaxDurationAnnotation: initialMaxDurationSeconds(sandbox.Spec.MaxDurationSeconds),
			},
			Labels: map[string]string{
				labelSandboxKey:   sandbox.Name,
				labelManagedByKey: managedByValue,
			},
		},
		Spec: corev1.PodSpec{
			ShareProcessNamespace: &shareProcessNamespace,
			RestartPolicy:         corev1.RestartPolicyNever,
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

	var initContainers []corev1.Container
	if sandbox.Spec.Workspace.Git != nil && sandbox.Spec.Workspace.Git.URL != "" {
		initContainers = append(initContainers, r.buildGitInitContainer(sandbox, mountPath))
	}
	if len(sandbox.Spec.MCPServers) > 0 {
		for _, server := range sandbox.Spec.MCPServers {
			initContainers = append(initContainers, r.buildMCPSidecarContainer(server, mountPath, workspaceReadOnly))
		}
	}
	if len(initContainers) > 0 {
		pod.Spec.InitContainers = initContainers
	}

	if sandbox.Spec.MaxDurationSeconds != nil {
		pod.Spec.ActiveDeadlineSeconds = sandbox.Spec.MaxDurationSeconds
	}

	return pod
}

func mcpScratchVolumeName(serverName string) string {
	prefix := "tmp-mcp-"
	if len(prefix)+len(serverName) <= 63 {
		return prefix + serverName
	}
	hash := sha256.Sum256([]byte(serverName))
	hashHex := hex.EncodeToString(hash[:])[:8]
	maxLen := 63 - len(prefix) - 1 - len(hashHex)
	return fmt.Sprintf("%s%s-%s", prefix, serverName[:maxLen], hashHex)
}

func (r *SandboxReconciler) buildMCPSidecarContainer(
	server sandboxesv1alpha1.MCPServerSpec,
	mountPath string,
	workspaceReadOnly bool,
) corev1.Container {
	boolFalse := false
	boolTrue := true
	uid := nonRootUID
	restartPolicyAlways := corev1.ContainerRestartPolicyAlways

	tmpVolumeName := mcpScratchVolumeName(server.Name)
	sidecarMounts := []corev1.VolumeMount{
		{Name: tmpVolumeName, MountPath: "/tmp"},
	}

	if server.MountWorkspace {
		sidecarReadOnly := server.MountWorkspaceReadOnly || workspaceReadOnly
		sidecarMounts = append(sidecarMounts, corev1.VolumeMount{
			Name:      "workspace-volume",
			MountPath: mountPath,
			ReadOnly:  sidecarReadOnly,
		})
	}

	sidecarEnv := []corev1.EnvVar{
		{Name: "HOME", Value: "/tmp"},
	}
	sidecarEnv = append(sidecarEnv, server.Env...)

	startupProbe := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{
				Port: intstr.FromInt32(server.Port),
			},
		},
		InitialDelaySeconds: 0,
		PeriodSeconds:       1,
		TimeoutSeconds:      1,
		SuccessThreshold:    1,
		FailureThreshold:    30,
	}

	return corev1.Container{
		Name:            server.Name,
		Image:           server.Image,
		Command:         server.Command,
		Args:            server.Args,
		Env:             sidecarEnv,
		Resources:       server.Resources,
		VolumeMounts:    sidecarMounts,
		ImagePullPolicy: corev1.PullIfNotPresent,
		RestartPolicy:   &restartPolicyAlways,
		StartupProbe:    startupProbe,
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
}

func mcpDiscoveryEnvVars(mcpServers []sandboxesv1alpha1.MCPServerSpec) []corev1.EnvVar {
	if len(mcpServers) == 0 {
		return nil
	}
	envs := make([]corev1.EnvVar, 0, len(mcpServers)*2+1)
	names := make([]string, 0, len(mcpServers))
	for _, s := range mcpServers {
		names = append(names, s.Name)
		normalized := strings.ToUpper(strings.ReplaceAll(s.Name, "-", "_"))
		envs = append(envs,
			corev1.EnvVar{
				Name:  fmt.Sprintf("BUDDY_MCP_%s_URL", normalized),
				Value: fmt.Sprintf("http://127.0.0.1:%d", s.Port),
			},
			corev1.EnvVar{
				Name:  fmt.Sprintf("BUDDY_MCP_%s_PORT", normalized),
				Value: strconv.FormatInt(int64(s.Port), 10),
			},
		)
	}
	envs = append(envs, corev1.EnvVar{
		Name:  "BUDDY_MCP_SERVERS",
		Value: strings.Join(names, ","),
	})
	return envs
}

func initialMaxDurationSeconds(duration *int64) string {
	if duration == nil {
		return "0"
	}
	return strconv.FormatInt(*duration, 10)
}

func downwardAPIEnvVars() []corev1.EnvVar {
	fieldEnv := func(name, fieldPath string) corev1.EnvVar {
		return corev1.EnvVar{
			Name: name,
			ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{
				APIVersion: "v1",
				FieldPath:  fieldPath,
			}},
		}
	}
	resourceEnv := func(name, resourceName, divisor string) corev1.EnvVar {
		return corev1.EnvVar{
			Name: name,
			ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{
				ContainerName: "agent",
				Resource:      resourceName,
				Divisor:       resource.MustParse(divisor),
			}},
		}
	}
	return []corev1.EnvVar{
		fieldEnv(sandboxesv1alpha1.AgentEnvSandboxName, "metadata.labels['"+labelSandboxKey+"']"),
		fieldEnv(sandboxesv1alpha1.AgentEnvPodName, "metadata.name"),
		fieldEnv(sandboxesv1alpha1.AgentEnvPodNamespace, "metadata.namespace"),
		fieldEnv(sandboxesv1alpha1.AgentEnvPodUID, "metadata.uid"),
		resourceEnv(sandboxesv1alpha1.AgentEnvCPURequestMillicores, "requests.cpu", "1m"),
		resourceEnv(sandboxesv1alpha1.AgentEnvCPULimitMillicores, "limits.cpu", "1m"),
		resourceEnv(sandboxesv1alpha1.AgentEnvMemoryRequestBytes, "requests.memory", "1"),
		resourceEnv(sandboxesv1alpha1.AgentEnvMemoryLimitBytes, "limits.memory", "1"),
		fieldEnv(sandboxesv1alpha1.AgentEnvInitialMaxDurationSeconds, "metadata.annotations['"+sandboxesv1alpha1.InitialMaxDurationAnnotation+"']"),
	}
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

if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  echo "Git repository already exists in $TARGET"
  exit 0
fi

if [ -n "$(find . -mindepth 1 -maxdepth 1 -print -quit)" ]; then
  echo "Refusing to clone into nonempty directory $TARGET because it is not a valid Git working tree" >&2
  exit 1
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
	if len(sandbox.Spec.Network.AllowedDomains) > 0 {
		envVars = append(envVars, r.proxyEnvVars(sandbox)...)
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

func checkPendingPodErrors(sandbox *sandboxesv1alpha1.Sandbox, pod *corev1.Pod, now metav1.Time, status *sandboxesv1alpha1.SandboxStatus) bool {
	mcpSidecars := make(map[string]struct{}, len(sandbox.Spec.MCPServers))
	for _, s := range sandbox.Spec.MCPServers {
		mcpSidecars[s.Name] = struct{}{}
	}

	// Check for init container waiting errors or failures
	for _, ics := range pod.Status.InitContainerStatuses {
		_, isNativeSidecar := mcpSidecars[ics.Name]

		if ics.State.Waiting != nil {
			reason := ics.State.Waiting.Reason
			if reason == "ImagePullBackOff" || reason == reasonErrImagePull || reason == reasonCrashLoopBackOff {
				status.Phase = sandboxesv1alpha1.SandboxPhaseFailed
				status.CompletionTime = &now
				failReason := reason
				if isNativeSidecar && reason == reasonCrashLoopBackOff {
					failReason = "SidecarStartupFailed"
				}
				meta.SetStatusCondition(&status.Conditions, metav1.Condition{
					Type:               sandboxesv1alpha1.ConditionReady,
					Status:             metav1.ConditionFalse,
					Reason:             failReason,
					Message:            fmt.Sprintf("Init container %s waiting: %s", ics.Name, ics.State.Waiting.Message),
					LastTransitionTime: now,
				})
				return true
			}
		}
		// Standard init containers fail on non-zero exit; native sidecars are restarted by kubelet.
		if !isNativeSidecar && ics.State.Terminated != nil && ics.State.Terminated.ExitCode != 0 {
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
			if reason == "ImagePullBackOff" || reason == reasonErrImagePull || reason == reasonCrashLoopBackOff {
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
			if !hasDeadlinePolicyMismatch(status) {
				meta.SetStatusCondition(&status.Conditions, metav1.Condition{
					Type:               sandboxesv1alpha1.ConditionReady,
					Status:             metav1.ConditionFalse,
					Reason:             cond.Reason,
					Message:            cond.Message,
					LastTransitionTime: now,
				})
			}
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

func checkSidecarDegradation(sandbox *sandboxesv1alpha1.Sandbox, pod *corev1.Pod) (string, string) {
	if len(sandbox.Spec.MCPServers) == 0 {
		return "", ""
	}
	mcpSidecars := make(map[string]struct{}, len(sandbox.Spec.MCPServers))
	for _, s := range sandbox.Spec.MCPServers {
		mcpSidecars[s.Name] = struct{}{}
	}

	for _, ics := range pod.Status.InitContainerStatuses {
		if _, ok := mcpSidecars[ics.Name]; !ok {
			continue
		}
		if ics.State.Waiting != nil && (ics.State.Waiting.Reason == reasonCrashLoopBackOff || ics.State.Waiting.Reason == reasonErrImagePull) {
			return ics.Name, fmt.Sprintf("MCP sidecar '%s' is crashing (%s: %s)", ics.Name, ics.State.Waiting.Reason, ics.State.Waiting.Message)
		}
		if !ics.Ready && ics.State.Running == nil && ics.RestartCount > 0 {
			return ics.Name, fmt.Sprintf("MCP sidecar '%s' is not ready (restarts: %d)", ics.Name, ics.RestartCount)
		}
	}
	return "", ""
}

func (r *SandboxReconciler) markSandboxSucceeded(sandbox *sandboxesv1alpha1.Sandbox, now metav1.Time) {
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
}

func (r *SandboxReconciler) markSandboxFailed(sandbox *sandboxesv1alpha1.Sandbox, now metav1.Time, exitCode *int32, reason, message string) {
	sandbox.Status.Phase = sandboxesv1alpha1.SandboxPhaseFailed
	if sandbox.Status.CompletionTime == nil {
		sandbox.Status.CompletionTime = &now
	}
	sandbox.Status.ExitCode = exitCode

	condReason := reason
	if condReason == "" {
		condReason = "PodFailed"
	}
	condMessage := message
	if condMessage == "" {
		if exitCode != nil {
			condMessage = fmt.Sprintf("Sandbox container failed with exit code %d", *exitCode)
		} else {
			condMessage = "Sandbox pod failed"
		}
	}

	meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
		Type:               sandboxesv1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             condReason,
		Message:            condMessage,
		LastTransitionTime: now,
	})
}

func (r *SandboxReconciler) observePodStatus(sandbox *sandboxesv1alpha1.Sandbox, pod *corev1.Pod) {
	now := metav1.Now()
	sandbox.Status.PodName = pod.Name
	sandbox.Status.PodUID = string(pod.UID)
	if sandbox.Status.FirstPodStartTime == nil && pod.Status.StartTime != nil {
		firstStart := metav1.NewTime(pod.Status.StartTime.Time)
		sandbox.Status.FirstPodStartTime = &firstStart
	}
	deadlineMismatch := hasDeadlinePolicyMismatch(&sandbox.Status)
	agentExitCode := extractAgentExitCode(pod)

	switch pod.Status.Phase {
	case corev1.PodPending:
		if checkPendingPodErrors(sandbox, pod, now, &sandbox.Status) {
			return
		}

		sandbox.Status.Phase = sandboxesv1alpha1.SandboxPhasePending
		if !deadlineMismatch {
			meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
				Type:               sandboxesv1alpha1.ConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             "PodPending",
				Message:            "Sandbox container is initializing or waiting for image pull",
				LastTransitionTime: now,
			})
		}
		return

	case corev1.PodRunning:
		if agentExitCode != nil {
			if *agentExitCode == 0 {
				r.markSandboxSucceeded(sandbox, now)
				return
			}
			r.markSandboxFailed(sandbox, now, agentExitCode, pod.Status.Reason, pod.Status.Message)
			return
		}

		sandbox.Status.Phase = sandboxesv1alpha1.SandboxPhaseRunning
		if sandbox.Status.SuspensionPhase == sandboxesv1alpha1.SuspensionPhaseResuming {
			sandbox.Status.SuspensionPhase = sandboxesv1alpha1.SuspensionPhaseNone
			setSuspensionCondition(sandbox, true, "Active", "Sandbox resumed and is running")
		}
		if !deadlineMismatch {
			if degradedSidecar, msg := checkSidecarDegradation(sandbox, pod); degradedSidecar != "" {
				meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
					Type:               sandboxesv1alpha1.ConditionReady,
					Status:             metav1.ConditionFalse,
					Reason:             "SidecarDegraded",
					Message:            msg,
					LastTransitionTime: now,
				})
			} else {
				meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
					Type:               sandboxesv1alpha1.ConditionReady,
					Status:             metav1.ConditionTrue,
					Reason:             "PodRunning",
					Message:            "Sandbox pod is active and running",
					LastTransitionTime: now,
				})
			}
		}
		return

	case corev1.PodSucceeded:
		r.markSandboxSucceeded(sandbox, now)
		return

	case corev1.PodFailed:
		// Agent exit code takes precedence over sidecar shutdown signals.
		if agentExitCode != nil && *agentExitCode == 0 {
			r.markSandboxSucceeded(sandbox, now)
			return
		}

		r.markSandboxFailed(sandbox, now, agentExitCode, pod.Status.Reason, pod.Status.Message)
		return
	}
}

func hasDeadlinePolicyMismatch(status *sandboxesv1alpha1.SandboxStatus) bool {
	condition := meta.FindStatusCondition(status.Conditions, sandboxesv1alpha1.ConditionDeadlinePolicyReady)
	return condition != nil && condition.Status == metav1.ConditionFalse && condition.Reason == sandboxesv1alpha1.ReasonDeadlinePolicyMismatch
}

func statusEqual(a, b *sandboxesv1alpha1.SandboxStatus) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Phase != b.Phase ||
		a.PodName != b.PodName ||
		a.PodUID != b.PodUID ||
		a.SuspensionPhase != b.SuspensionPhase ||
		a.NetworkPolicyName != b.NetworkPolicyName ||
		a.PVCName != b.PVCName {
		return false
	}
	if (a.FirstPodStartTime == nil) != (b.FirstPodStartTime == nil) {
		return false
	}
	if a.FirstPodStartTime != nil && !a.FirstPodStartTime.Time.Equal(b.FirstPodStartTime.Time) {
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
