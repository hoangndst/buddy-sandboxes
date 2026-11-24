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

const (
	LabelPoolName   = "sandboxes.buddy.io/pool"
	LabelPoolStatus = "sandboxes.buddy.io/pool-status"
	LabelLeaseName  = "sandboxes.buddy.io/lease"

	PoolStatusStandby = "standby"
	PoolStatusClaimed = "claimed"
	PoolStatusBound   = "bound"

	AnnotationClaimedAt     = "sandboxes.buddy.io/claimed-at"
	AnnotationClaimDeadline = "sandboxes.buddy.io/claim-deadline"

	ConditionPoolReady   = "Ready"
	ConditionPoolScaling = "Scaling"
)

// SandboxTemplateSpec defines the container blueprint for warm sandbox runners.
type SandboxTemplateSpec struct {
	// Image is the container image to run.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Command is the entrypoint array. Defaults to an idle keepalive if omitted.
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
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// NetworkProfileRef references an independent NetworkProfile custom resource.
	// +optional
	NetworkProfileRef *corev1.LocalObjectReference `json:"networkProfileRef,omitempty"`
}

// SandboxPoolSpec defines the desired state of SandboxPool.
// +kubebuilder:validation:XValidation:rule="self.maxWarm >= self.minWarm",message="maxWarm must be greater than or equal to minWarm"
type SandboxPoolSpec struct {
	// MinWarm is the minimum number of ready standby runners maintained in the pool.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=2
	MinWarm int32 `json:"minWarm"`

	// MaxWarm is the maximum total runners (standby + claimed) allowed across the pool.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=10
	MaxWarm int32 `json:"maxWarm"`

	// IdleTimeoutSeconds is the maximum idle age before a surplus standby runner is recycled.
	// +kubebuilder:default=600
	// +kubebuilder:validation:Minimum=10
	// +optional
	IdleTimeoutSeconds int32 `json:"idleTimeoutSeconds,omitempty"`

	// Template defines the container blueprint for runners spawned by this pool.
	// +kubebuilder:validation:Required
	Template SandboxTemplateSpec `json:"template"`
}

// SandboxPoolStatus defines the observed state of SandboxPool.
type SandboxPoolStatus struct {
	// ReadyReplicas is the number of available standby runners ready for immediate lease.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas"`

	// AllocatedReplicas is the number of runners currently claimed by active leases.
	// +optional
	AllocatedReplicas int32 `json:"allocatedReplicas"`

	// TotalReplicas is the total number of child runners managed by the pool.
	// +optional
	TotalReplicas int32 `json:"totalReplicas"`

	// Conditions represent observations of current pool operational state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sp;pool
// +kubebuilder:printcolumn:name="MinWarm",type="integer",JSONPath=".spec.minWarm"
// +kubebuilder:printcolumn:name="MaxWarm",type="integer",JSONPath=".spec.maxWarm"
// +kubebuilder:printcolumn:name="Ready",type="integer",JSONPath=".status.readyReplicas"
// +kubebuilder:printcolumn:name="Allocated",type="integer",JSONPath=".status.allocatedReplicas"
// +kubebuilder:printcolumn:name="Total",type="integer",JSONPath=".status.totalReplicas"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// SandboxPool is the Schema for the sandboxpools API.
type SandboxPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxPoolSpec   `json:"spec,omitempty"`
	Status SandboxPoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SandboxPoolList contains a list of SandboxPool.
type SandboxPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxPool{}, &SandboxPoolList{})
}
