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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SandboxPhase represents the current phase of the sandbox execution.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
type SandboxPhase string

const (
	// SandboxPhasePending indicates that resources are being provisioned.
	SandboxPhasePending SandboxPhase = "Pending"
	// SandboxPhaseRunning indicates that the sandbox pod is active and running.
	SandboxPhaseRunning SandboxPhase = "Running"
	// SandboxPhaseSucceeded indicates that the sandbox process finished with exit code 0.
	SandboxPhaseSucceeded SandboxPhase = "Succeeded"
	// SandboxPhaseFailed indicates that the sandbox failed, timed out, or exited with non-zero code.
	SandboxPhaseFailed SandboxPhase = "Failed"
)

// Standard condition types for Sandbox status.
const (
	ConditionInitialized         string = "Initialized"
	ConditionNetworkConfigured   string = "NetworkConfigured"
	ConditionStorageConfigured   string = "StorageConfigured"
	ConditionRuntimeClassReady   string = "RuntimeClassReady"
	ConditionDeadlinePolicyReady string = "DeadlinePolicyReady"
	ConditionReady               string = "Ready"

	ReasonRuntimeClassValid       string = "RuntimeClassValid"
	ReasonRuntimeClassUnavailable string = "RuntimeClassUnavailable"
	ReasonDeadlinePolicyMatched   string = "Matched"
	ReasonDeadlinePolicyPatched   string = "Patched"
	ReasonDeadlinePolicyMismatch  string = "DeadlinePolicyMismatch"
)

const (
	MaxActiveDeadlineSeconds int64 = 2147483647

	AgentEnvSandboxName               = "BUDDY_SANDBOX_NAME"
	AgentEnvPodName                   = "BUDDY_POD_NAME"
	AgentEnvPodNamespace              = "BUDDY_POD_NAMESPACE"
	AgentEnvPodUID                    = "BUDDY_POD_UID"
	AgentEnvCPURequestMillicores      = "BUDDY_CPU_REQUEST_MILLICORES"
	AgentEnvCPULimitMillicores        = "BUDDY_CPU_LIMIT_MILLICORES"
	AgentEnvMemoryRequestBytes        = "BUDDY_MEMORY_REQUEST_BYTES"
	AgentEnvMemoryLimitBytes          = "BUDDY_MEMORY_LIMIT_BYTES"
	AgentEnvInitialMaxDurationSeconds = "BUDDY_INITIAL_MAX_DURATION_SECONDS"
	InitialMaxDurationAnnotation      = "sandboxes.buddy.io/initial-max-duration-seconds"
)

// IsReservedAgentEnvName reports whether a Sandbox env entry collides with a controller-provided value.
func IsReservedAgentEnvName(name string) bool {
	switch name {
	case AgentEnvSandboxName, AgentEnvPodName, AgentEnvPodNamespace, AgentEnvPodUID,
		AgentEnvCPURequestMillicores, AgentEnvCPULimitMillicores,
		AgentEnvMemoryRequestBytes, AgentEnvMemoryLimitBytes, AgentEnvInitialMaxDurationSeconds:
		return true
	default:
		return false
	}
}

// GitRepositorySpec defines declarative repository cloning parameters into the workspace.
type GitRepositorySpec struct {
	// URL is the HTTP(S) or SSH git repository URL.
	// +kubebuilder:validation:Required
	URL string `json:"url"`

	// Revision is the branch, tag, or commit hash to check out (defaults to HEAD).
	// +optional
	Revision string `json:"revision,omitempty"`

	// Depth is the shallow clone depth (e.g. 1). Omit or 0 for full clone.
	// +optional
	Depth int32 `json:"depth,omitempty"`

	// Subpath is the destination subfolder relative to mountPath.
	// +optional
	Subpath string `json:"subpath,omitempty"`

	// SecretRef references a Secret in the same namespace containing credentials (e.g. token or SSH key).
	// +optional
	SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`
}

// PersistentVolumeClaimWorkspace defines a pre-existing or external PVC to mount.
type PersistentVolumeClaimWorkspace struct {
	// ClaimName is the name of the PersistentVolumeClaim in the same namespace.
	// +kubebuilder:validation:Required
	ClaimName string `json:"claimName"`

	// ReadOnly specifies whether to mount the volume as read-only.
	// +optional
	ReadOnly bool `json:"readOnly,omitempty"`
}

// WorkspaceSpec defines the storage configuration for the agent workspace.
type WorkspaceSpec struct {
	// MountPath is the path where the workspace directory is mounted inside the container.
	// Defaults to /workspace.
	// +kubebuilder:default="/workspace"
	// +optional
	MountPath string `json:"mountPath,omitempty"`

	// Ephemeral indicates whether the workspace is backed by an emptyDir volume.
	// Defaults to true for fast provisioning.
	// +kubebuilder:default=true
	// +optional
	Ephemeral bool `json:"ephemeral,omitempty"`

	// Size is the requested volume size when using persistent storage (e.g. 10Gi).
	// +optional
	Size string `json:"size,omitempty"`

	// StorageClassName specifies the storage class to use when persistent storage is requested.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`

	// Git specifies repository seeding configuration cloned into the workspace via an init container.
	// +optional
	Git *GitRepositorySpec `json:"git,omitempty"`

	// PersistentVolumeClaim specifies an existing PVC to mount into the workspace.
	// +optional
	PersistentVolumeClaim *PersistentVolumeClaimWorkspace `json:"persistentVolumeClaim,omitempty"`
}

// NetworkSpec defines the network boundaries for the sandbox.
type NetworkSpec struct {
	// AllowInternetWeb permits outbound HTTP and HTTPS traffic to ports 80 and 443.
	// Defaults to true to allow git and package downloads.
	// +kubebuilder:default=true
	// +optional
	AllowInternetWeb bool `json:"allowInternetWeb,omitempty"`

	// AllowDNS permits outbound DNS traffic to CoreDNS.
	// Defaults to true.
	// +kubebuilder:default=true
	// +optional
	AllowDNS bool `json:"allowDNS,omitempty"`

	// BlockClusterInternal blocks egress to private RFC1918 subnets and cluster internal ranges.
	// Defaults to true.
	// +kubebuilder:default=true
	// +optional
	BlockClusterInternal bool `json:"blockClusterInternal,omitempty"`

	// BlockMetadataService blocks egress to cloud instance metadata at 169.254.169.254/32.
	// Defaults to true.
	// +kubebuilder:default=true
	// +optional
	BlockMetadataService bool `json:"blockMetadataService,omitempty"`
}

// SandboxSpec defines the desired state of Sandbox.
// +kubebuilder:validation:XValidation:rule="(!has(oldSelf.shareProcessNamespace) || oldSelf.shareProcessNamespace == false) == (!has(self.shareProcessNamespace) || self.shareProcessNamespace == false)",message="shareProcessNamespace is immutable after Sandbox creation"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.maxDurationSeconds) || (has(self.maxDurationSeconds) && self.maxDurationSeconds <= oldSelf.maxDurationSeconds)",message="maxDurationSeconds may only be set or reduced, not removed or increased"
type SandboxSpec struct {
	// Image is the container image to run in the sandbox.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Command is the entrypoint array.
	// +optional
	Command []string `json:"command,omitempty"`

	// Args is the arguments array for the entrypoint.
	// +optional
	Args []string `json:"args,omitempty"`

	// WorkingDir is the container working directory. Defaults to /workspace.
	// +kubebuilder:default="/workspace"
	// +optional
	WorkingDir string `json:"workingDir,omitempty"`

	// RuntimeClassName specifies the container runtime class (e.g. gvisor, kata, runc).
	// +optional
	RuntimeClassName *string `json:"runtimeClassName,omitempty"`

	// Resources describes compute resource requirements for the sandbox container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Env is the list of environment variables to set in the sandbox container.
	// +kubebuilder:validation:XValidation:rule="self.all(e, !(e.name in ['BUDDY_SANDBOX_NAME', 'BUDDY_POD_NAME', 'BUDDY_POD_NAMESPACE', 'BUDDY_POD_UID', 'BUDDY_CPU_REQUEST_MILLICORES', 'BUDDY_CPU_LIMIT_MILLICORES', 'BUDDY_MEMORY_REQUEST_BYTES', 'BUDDY_MEMORY_LIMIT_BYTES', 'BUDDY_INITIAL_MAX_DURATION_SECONDS']))",message="env contains a name reserved by the sandbox controller"
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// EnvFrom is the list of sources to populate environment variables in the container.
	// +optional
	EnvFrom []corev1.EnvFromSource `json:"envFrom,omitempty"`

	// Workspace configures storage for the sandbox working tree.
	// +optional
	Workspace WorkspaceSpec `json:"workspace,omitempty"`

	// Network configures egress filtering rules for the sandbox.
	// +kubebuilder:default={allowInternetWeb: true, allowDNS: true, blockClusterInternal: true, blockMetadataService: true}
	// +optional
	Network NetworkSpec `json:"network,omitempty"`

	// ShareProcessNamespace enables a shared process namespace within the Pod.
	// Defaults to false to preserve image PID 1 behavior. Immutable after creation.
	// +kubebuilder:default=false
	// +optional
	ShareProcessNamespace *bool `json:"shareProcessNamespace,omitempty"`

	// MaxDurationSeconds defines the execution deadline in seconds.
	// Maps to pod.spec.activeDeadlineSeconds.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=2147483647
	// +optional
	MaxDurationSeconds *int64 `json:"maxDurationSeconds,omitempty"`
}

// SandboxStatus defines the observed state of Sandbox.
type SandboxStatus struct {
	// Phase is the current execution phase of the sandbox.
	// +optional
	Phase SandboxPhase `json:"phase,omitempty"`

	// PodName is the name of the underlying Pod executing the sandbox.
	// +optional
	PodName string `json:"podName,omitempty"`

	// NetworkPolicyName is the name of the synthesized NetworkPolicy.
	// +optional
	NetworkPolicyName string `json:"networkPolicyName,omitempty"`

	// PVCName is the name of the PersistentVolumeClaim if persistent storage is used.
	// +optional
	PVCName string `json:"pvcName,omitempty"`

	// StartTime represents time when the sandbox was acknowledged and started by the controller.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime represents time when the sandbox completed execution.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// ExitCode is the exit status code of the primary container when finished.
	// +optional
	ExitCode *int32 `json:"exitCode,omitempty"`

	// Conditions represent the latest observations of the sandbox state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase",description="The current phase of the sandbox"
// +kubebuilder:printcolumn:name="Pod",type="string",JSONPath=".status.podName",description="The backing Pod name"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Sandbox is the Schema for the sandboxes API.
type Sandbox struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxSpec   `json:"spec,omitempty"`
	Status SandboxStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SandboxList contains a list of Sandbox.
type SandboxList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Sandbox `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Sandbox{}, &SandboxList{})
}
