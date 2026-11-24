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
	"strings"

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
	ConditionSuspensionReady     string = "SuspensionReady"
	ConditionReady               string = "Ready"

	ReasonRuntimeClassValid        string = "RuntimeClassValid"
	ReasonRuntimeClassUnavailable  string = "RuntimeClassUnavailable"
	ReasonDeadlinePolicyMatched    string = "Matched"
	ReasonDeadlinePolicyPatched    string = "Patched"
	ReasonDeadlinePolicyMismatch   string = "DeadlinePolicyMismatch"
	ReasonNetworkProfileNotReady   string = "NetworkProfileNotReady"
	ReasonNetworkProfileReferenced string = "NetworkProfileReferenced"
	ReasonNetworkPolicyCreated     string = "NetworkPolicyCreated"
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
	PodDeadlineSecondsAnnotation      = "sandboxes.buddy.io/pod-deadline-seconds"
)

// SuspensionPhase represents the observed suspend/resume lifecycle.
// +kubebuilder:validation:Enum=None;Suspending;Suspended;Resuming
type SuspensionPhase string

const (
	SuspensionPhaseNone       SuspensionPhase = "None"
	SuspensionPhaseSuspending SuspensionPhase = "Suspending"
	SuspensionPhaseSuspended  SuspensionPhase = "Suspended"
	SuspensionPhaseResuming   SuspensionPhase = "Resuming"
)

// IsReservedAgentEnvName reports whether a Sandbox env entry collides with a controller-provided value.
func IsReservedAgentEnvName(name string) bool {
	if strings.HasPrefix(name, "BUDDY_MCP_") {
		return true
	}
	switch name {
	case AgentEnvSandboxName, AgentEnvPodName, AgentEnvPodNamespace, AgentEnvPodUID,
		AgentEnvCPURequestMillicores, AgentEnvCPULimitMillicores,
		AgentEnvMemoryRequestBytes, AgentEnvMemoryLimitBytes, AgentEnvInitialMaxDurationSeconds,
		"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "NO_PROXY", "no_proxy":
		return true
	default:
		return false
	}
}

// MCPServerSpec configures an MCP sidecar container.
type MCPServerSpec struct {
	// Name is the sidecar container name. Max 55 characters.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=55
	// +kubebuilder:validation:Pattern="^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
	Name string `json:"name"`

	// Image is the container image to run.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Port is the TCP port where the sidecar listens for HTTP/SSE connections.
	// Must bind to 0.0.0.0 or the Pod IP for kubelet startup probes.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`

	// Command overrides the container entrypoint.
	// +optional
	Command []string `json:"command,omitempty"`

	// Args provides arguments to the entrypoint.
	// +optional
	Args []string `json:"args,omitempty"`

	// Env specifies environment variables passed to the container.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Resources defines compute resource requests and limits.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// MountWorkspace mounts the workspace volume at /workspace.
	// +kubebuilder:default=false
	// +optional
	MountWorkspace bool `json:"mountWorkspace,omitempty"`

	// MountWorkspaceReadOnly mounts the workspace read-only.
	// +kubebuilder:default=false
	// +optional
	MountWorkspaceReadOnly bool `json:"mountWorkspaceReadOnly,omitempty"`
}

// GitRepositorySpec defines declarative repository cloning parameters into the workspace.
// +kubebuilder:validation:XValidation:rule="!has(self.secretRef) || (self.url.matches('^https://[a-z0-9.-]+/[^?#%]+$') && !self.url.endsWith('/'))",message="credentialed Git repositories require a canonical HTTPS URL without userinfo, query, fragment, encoded path, or port"
// +kubebuilder:validation:XValidation:rule="!has(self.secretRef) || !self.url.matches('^https://[0-9.]+/.*$')",message="credentialed Git repositories must use a DNS hostname, not an IP address"
// +kubebuilder:validation:XValidation:rule="!self.url.matches('(?i)^https?://[^/]*@.*$')",message="Git repository URLs must not contain HTTP(S) userinfo"
type GitRepositorySpec struct {
	// URL is the HTTP(S) or SSH git repository URL.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=2048
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

	// SecretRef references a same-namespace, repository-bound HTTPS credential Secret with host, path, username, and token data keys.
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

// WorkspaceReclaimPolicy defines the retention policy for dynamically created persistent storage.
// +kubebuilder:validation:Enum=Retain;Delete
type WorkspaceReclaimPolicy string

const (
	// WorkspaceReclaimRetain preserves the underlying PersistentVolumeClaim when the Sandbox is deleted.
	WorkspaceReclaimRetain WorkspaceReclaimPolicy = "Retain"
	// WorkspaceReclaimDelete removes the underlying PersistentVolumeClaim via cascading deletion when the Sandbox is deleted.
	WorkspaceReclaimDelete WorkspaceReclaimPolicy = "Delete"
)

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
	Ephemeral *bool `json:"ephemeral,omitempty"`

	// Size is the requested volume size when using persistent storage (e.g. 10Gi).
	// +kubebuilder:validation:Pattern="^[0-9]+(\\.[0-9]+)?(E|P|T|G|M|k|Ei|Pi|Ti|Gi|Mi|Ki)?$"
	// +optional
	Size string `json:"size,omitempty"`

	// StorageClassName specifies the storage class to use when persistent storage is requested.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`

	// ReclaimPolicy defines whether dynamically created persistent storage is preserved (Retain)
	// or removed via cascading deletion (Delete) when the Sandbox custom resource is deleted.
	// Defaults to Retain to prevent accidental data loss.
	// +kubebuilder:validation:Enum=Retain;Delete
	// +kubebuilder:default=Retain
	// +optional
	ReclaimPolicy WorkspaceReclaimPolicy `json:"reclaimPolicy,omitempty"`

	// Git specifies repository seeding configuration cloned into the workspace via an init container.
	// +optional
	Git *GitRepositorySpec `json:"git,omitempty"`

	// PersistentVolumeClaim specifies an existing PVC to mount into the workspace.
	// +optional
	PersistentVolumeClaim *PersistentVolumeClaimWorkspace `json:"persistentVolumeClaim,omitempty"`
}

// NetworkSpec defines the network boundaries for the sandbox.
type NetworkSpec struct {
	// AllowAPIBroker permits access to the in-cluster provider credential broker.
	// +optional
	AllowAPIBroker bool `json:"allowAPIBroker,omitempty"`

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

	// AllowedDomains specifies declarative FQDNs and wildcard domains permitted for egress.
	// When populated, direct open internet access is blocked and traffic is routed strictly
	// through the buddy-egress-gateway.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:Pattern=`^(\*\.)?([a-zA-Z0-9-]+\.)+[a-zA-Z]{2,}$`
	AllowedDomains []string `json:"allowedDomains,omitempty"`
}

// SandboxSpec defines the desired state of Sandbox.
// +kubebuilder:validation:XValidation:rule="(!has(oldSelf.shareProcessNamespace) || oldSelf.shareProcessNamespace == false) == (!has(self.shareProcessNamespace) || self.shareProcessNamespace == false)",message="shareProcessNamespace is immutable after Sandbox creation"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.maxDurationSeconds) || (has(self.maxDurationSeconds) && self.maxDurationSeconds <= oldSelf.maxDurationSeconds)",message="maxDurationSeconds may only be set or reduced, not removed or increased"
// +kubebuilder:validation:XValidation:rule="(!has(oldSelf.mcpServers) && !has(self.mcpServers)) || (has(oldSelf.mcpServers) && has(self.mcpServers) && oldSelf.mcpServers == self.mcpServers)",message="mcpServers is immutable after creation"
// +kubebuilder:validation:XValidation:rule="(!has(oldSelf.network) && !has(self.network)) || (has(oldSelf.network) && has(self.network) && oldSelf.network == self.network)",message="network is immutable after creation"
// +kubebuilder:validation:XValidation:rule="(!has(oldSelf.workspace) && !has(self.workspace)) || (has(self.workspaceRef) && (!has(self.workspace) || (!has(self.workspace.size) && !has(self.workspace.persistentVolumeClaim) && !has(self.workspace.git)))) || (has(oldSelf.workspace) && has(self.workspace) && oldSelf.workspace == self.workspace)",message="workspace is immutable after creation"
// +kubebuilder:validation:XValidation:rule="(!has(oldSelf.workspaceRef) && !has(self.workspaceRef)) || (has(oldSelf.workspaceRef) && has(self.workspaceRef) && oldSelf.workspaceRef == self.workspaceRef)",message="workspaceRef is immutable after Sandbox creation"
// +kubebuilder:validation:XValidation:rule="(!has(oldSelf.networkProfileRef) && !has(self.networkProfileRef)) || (has(oldSelf.networkProfileRef) && has(self.networkProfileRef) && oldSelf.networkProfileRef == self.networkProfileRef)",message="networkProfileRef is immutable after Sandbox creation"
// +kubebuilder:validation:XValidation:rule="!(has(self.workspaceRef) && has(self.workspace) && (has(self.workspace.size) || has(self.workspace.persistentVolumeClaim) || has(self.workspace.git)))",message="cannot specify both workspaceRef and inline workspace storage/git configuration"
// +kubebuilder:validation:XValidation:rule="!(has(self.networkProfileRef) && has(self.network) && (has(self.network.allowedDomains) && size(self.network.allowedDomains) > 0))",message="cannot specify both networkProfileRef and inline network allowedDomains"
// +kubebuilder:validation:XValidation:rule="!has(self.suspend) || !self.suspend || has(self.workspaceRef) || (has(self.workspace) && ((has(self.workspace.persistentVolumeClaim) && (!has(self.workspace.persistentVolumeClaim.readOnly) || !self.workspace.persistentVolumeClaim.readOnly)) || (has(self.workspace.ephemeral) && !self.workspace.ephemeral && has(self.workspace.size) && size(self.workspace.size) > 0)))",message="suspension requires a writable PVC-backed workspace or workspaceRef"
// +kubebuilder:validation:XValidation:rule="!has(self.network) || (has(self.network.allowInternetWeb) && !self.network.allowInternetWeb ? !has(self.network.allowedDomains) || size(self.network.allowedDomains) == 0 : true)",message="allowedDomains cannot be configured when allowInternetWeb is false"
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
	// +kubebuilder:validation:XValidation:rule="self.all(e, !(e.name in ['BUDDY_SANDBOX_NAME', 'BUDDY_POD_NAME', 'BUDDY_POD_NAMESPACE', 'BUDDY_POD_UID', 'BUDDY_CPU_REQUEST_MILLICORES', 'BUDDY_CPU_LIMIT_MILLICORES', 'BUDDY_MEMORY_REQUEST_BYTES', 'BUDDY_MEMORY_LIMIT_BYTES', 'BUDDY_INITIAL_MAX_DURATION_SECONDS', 'HTTP_PROXY', 'HTTPS_PROXY', 'http_proxy', 'https_proxy', 'NO_PROXY', 'no_proxy']) && !e.name.startsWith('BUDDY_MCP_'))",message="env contains a name reserved by the sandbox controller"
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// EnvFrom is the list of sources to populate environment variables in the container.
	// +optional
	EnvFrom []corev1.EnvFromSource `json:"envFrom,omitempty"`

	// Workspace configures storage for the sandbox working tree.
	// +optional
	Workspace WorkspaceSpec `json:"workspace,omitempty"`

	// WorkspaceRef references an independent Workspace custom resource.
	// If set, takes precedence over inline Workspace.
	// +optional
	WorkspaceRef *corev1.LocalObjectReference `json:"workspaceRef,omitempty"`

	// Suspend requests cold suspension of a writable PVC-backed workspace.
	// +kubebuilder:default=false
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// Network configures egress filtering rules for the sandbox.
	// +kubebuilder:default={allowInternetWeb: true, allowDNS: true, blockClusterInternal: true, blockMetadataService: true}
	// +optional
	Network NetworkSpec `json:"network,omitempty"`

	// NetworkProfileRef references an independent NetworkProfile custom resource.
	// If set, takes precedence over inline Network.
	// +optional
	NetworkProfileRef *corev1.LocalObjectReference `json:"networkProfileRef,omitempty"`

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

	// MCPServers defines optional MCP sidecar containers.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:XValidation:rule="self.all(s, self.filter(o, o.name == s.name).size() == 1)",message="mcpServers names must be unique"
	// +kubebuilder:validation:XValidation:rule="self.all(s, self.filter(o, o.port == s.port).size() == 1)",message="mcpServers ports must be unique"
	// +kubebuilder:validation:XValidation:rule="self.all(s, !(s.name in ['agent', 'git-init']))",message="mcpServers name collides with reserved container names"
	// +listType=map
	// +listMapKey=name
	// +optional
	MCPServers []MCPServerSpec `json:"mcpServers,omitempty"`
}

// SandboxStatus defines the observed state of Sandbox.
type SandboxStatus struct {
	// Phase is the current execution phase of the sandbox.
	// +optional
	Phase SandboxPhase `json:"phase,omitempty"`

	// PodName is the name of the underlying Pod executing the sandbox.
	// +optional
	PodName string `json:"podName,omitempty"`

	// PodUID identifies the current execution Pod generation.
	// +optional
	PodUID string `json:"podUID,omitempty"`

	// SuspensionPhase reports the observed suspend/resume lifecycle.
	// +kubebuilder:default=None
	// +optional
	SuspensionPhase SuspensionPhase `json:"suspensionPhase,omitempty"`

	// NetworkPolicyName is the name of the synthesized NetworkPolicy.
	// +optional
	NetworkPolicyName string `json:"networkPolicyName,omitempty"`

	// PVCName is the name of the PersistentVolumeClaim if persistent storage is used.
	// +optional
	PVCName string `json:"pvcName,omitempty"`

	// StartTime represents time when the sandbox was acknowledged and started by the controller.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// FirstPodStartTime is the write-once start time used for the Sandbox execution deadline.
	// +optional
	FirstPodStartTime *metav1.Time `json:"firstPodStartTime,omitempty"`

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
