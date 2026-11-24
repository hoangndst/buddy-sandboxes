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

// WorkspacePhase represents the observed lifecycle phase of the workspace.
// +kubebuilder:validation:Enum=Pending;Ready;Bound;Failed
type WorkspacePhase string

const (
	// WorkspacePhasePending indicates that the workspace storage is being provisioned.
	WorkspacePhasePending WorkspacePhase = "Pending"
	// WorkspacePhaseReady indicates that the workspace storage is bound and ready for attachment.
	WorkspacePhaseReady WorkspacePhase = "Ready"
	// WorkspacePhaseBound indicates that the workspace is actively mounted by an executing Sandbox.
	WorkspacePhaseBound WorkspacePhase = "Bound"
	// WorkspacePhaseFailed indicates that storage provisioning failed.
	WorkspacePhaseFailed WorkspacePhase = "Failed"
)

// WorkspaceStatus defines the observed state of Workspace.
type WorkspaceStatus struct {
	// Phase is the current lifecycle phase of the workspace.
	// +optional
	Phase WorkspacePhase `json:"phase,omitempty"`

	// PVCName is the name of the underlying PersistentVolumeClaim.
	// +optional
	PVCName string `json:"pvcName,omitempty"`

	// Capacity is the actual allocated capacity of the underlying volume.
	// +optional
	Capacity string `json:"capacity,omitempty"`

	// BoundSandboxRef references the active Sandbox currently mounting this workspace.
	// +optional
	BoundSandboxRef *corev1.LocalObjectReference `json:"boundSandboxRef,omitempty"`

	// Conditions represent observations of current state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ws
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="PVC",type="string",JSONPath=".status.pvcName"
// +kubebuilder:printcolumn:name="Capacity",type="string",JSONPath=".status.capacity"
// +kubebuilder:printcolumn:name="BoundSandbox",type="string",JSONPath=".status.boundSandboxRef.name"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:validation:XValidation:rule="self.spec.reclaimPolicy == oldSelf.spec.reclaimPolicy",message="spec.reclaimPolicy is immutable after creation"

// Workspace is the Schema for the workspaces API.
type Workspace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkspaceSpec   `json:"spec,omitempty"`
	Status WorkspaceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WorkspaceList contains a list of Workspace.
type WorkspaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Workspace `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Workspace{}, &WorkspaceList{})
}
