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
	LeaseProtectionFinalizer = "sandboxes.buddy.io/lease-protection"

	AnnotationLeaseName = "sandboxes.buddy.io/lease-name"

	ConditionLeaseBound   = "Bound"
	ConditionLeaseExpired = "Expired"
)

// SandboxLeasePhase represents the current phase of a sandbox lease.
// +kubebuilder:validation:Enum=Pending;Bound;Released;Expired;Failed
type SandboxLeasePhase string

const (
	SandboxLeasePhasePending  SandboxLeasePhase = "Pending"
	SandboxLeasePhaseBound    SandboxLeasePhase = "Bound"
	SandboxLeasePhaseReleased SandboxLeasePhase = "Released"
	SandboxLeasePhaseExpired  SandboxLeasePhase = "Expired"
	SandboxLeasePhaseFailed   SandboxLeasePhase = "Failed"
)

// SandboxLeaseSpec defines the desired claim configuration.
// +kubebuilder:validation:XValidation:rule="self.poolRef == oldSelf.poolRef",message="poolRef is immutable after creation"
// +kubebuilder:validation:XValidation:rule="self.executionId == oldSelf.executionId",message="executionId is immutable after creation"
// +kubebuilder:validation:XValidation:rule="self.timeoutSeconds == oldSelf.timeoutSeconds",message="timeoutSeconds is immutable after creation"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.released) || oldSelf.released == false || (has(self.released) && self.released == true)",message="released cannot be set back to false once true"
type SandboxLeaseSpec struct {
	// PoolRef references the target SandboxPool to claim a runner from.
	// +kubebuilder:validation:Required
	PoolRef corev1.LocalObjectReference `json:"poolRef"`

	// ExecutionId is a unique task or client execution identifier. Max 63 characters.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	ExecutionId string `json:"executionId"`

	// TimeoutSeconds is the maximum lease duration in seconds before automatic expiration.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=86400
	// +kubebuilder:default=300
	TimeoutSeconds int32 `json:"timeoutSeconds"`

	// QueueTimeoutSeconds is the maximum duration in seconds a lease can wait in Pending state.
	// Defaults to 60.
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=5
	// +kubebuilder:validation:Maximum=3600
	// +optional
	QueueTimeoutSeconds int32 `json:"queueTimeoutSeconds,omitempty"`

	// Released indicates that the client has completed execution and released the runner early.
	// +kubebuilder:default=false
	// +optional
	Released bool `json:"released,omitempty"`
}

// SandboxLeaseStatus defines the observed state of SandboxLease.
type SandboxLeaseStatus struct {
	// Phase is the current lifecycle state of the lease.
	// +optional
	Phase SandboxLeasePhase `json:"phase,omitempty"`

	// BoundSandboxRef is a reference to the claimed child Sandbox.
	// +optional
	BoundSandboxRef *corev1.LocalObjectReference `json:"boundSandboxRef,omitempty"`

	// BoundPodName is the name of the active container pod executing the sandbox.
	// +optional
	BoundPodName string `json:"boundPodName,omitempty"`

	// LeasedAt is the timestamp when the standby runner was bound.
	// +optional
	LeasedAt *metav1.Time `json:"leasedAt,omitempty"`

	// ExpiresAt is the calculated expiration deadline.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`

	// Conditions represent observations of current lease state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sbl;lease
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Pool",type="string",JSONPath=".spec.poolRef.name"
// +kubebuilder:printcolumn:name="Sandbox",type="string",JSONPath=".status.boundSandboxRef.name"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// SandboxLease is the Schema for the sandboxleases API.
type SandboxLease struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SandboxLeaseSpec   `json:"spec,omitempty"`
	Status SandboxLeaseStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SandboxLeaseList contains a list of SandboxLease.
type SandboxLeaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SandboxLease `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SandboxLease{}, &SandboxLeaseList{})
}
