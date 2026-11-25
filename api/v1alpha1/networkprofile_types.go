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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NetworkIsolationLevel defines the high-level isolation preset.
// +kubebuilder:validation:Enum=HardPerimeter;DefaultDenyAll;PermissiveDev
type NetworkIsolationLevel string

const (
	// IsolationHardPerimeter isolates internal cluster subnets and cloud metadata while permitting egress proxying.
	IsolationHardPerimeter NetworkIsolationLevel = "HardPerimeter"
	// IsolationDefaultDenyAll blocks all outbound network traffic by default.
	IsolationDefaultDenyAll NetworkIsolationLevel = "DefaultDenyAll"
	// IsolationPermissiveDev allows standard developer networking without egress restrictions.
	IsolationPermissiveDev NetworkIsolationLevel = "PermissiveDev"
)

// NetworkProfileSpec defines the desired network boundaries and egress policies.
// +kubebuilder:validation:XValidation:rule="has(self.allowInternetWeb) && !self.allowInternetWeb ? !has(self.allowedDomains) || size(self.allowedDomains) == 0 : true",message="allowedDomains cannot be configured when allowInternetWeb is false"
// +kubebuilder:validation:XValidation:rule="self.isolationLevel == 'DefaultDenyAll' && (!has(self.allowInternetWeb) || !self.allowInternetWeb) ? !has(self.allowedDomains) || size(self.allowedDomains) == 0 : true",message="allowedDomains cannot be configured under DefaultDenyAll unless allowInternetWeb is explicitly enabled"
type NetworkProfileSpec struct {
	// IsolationLevel configures preset network boundaries. Defaults to HardPerimeter.
	// +kubebuilder:default=HardPerimeter
	// +optional
	IsolationLevel NetworkIsolationLevel `json:"isolationLevel,omitempty"`

	// AllowedDomains specifies declarative FQDNs and wildcard domains permitted for egress.
	// When specified, outbound traffic routes through the egress proxy gateway.
	// +listType=set
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:Pattern=`^(\*\.)?([a-zA-Z0-9-]+\.)+[a-zA-Z]{2,}$`
	// +optional
	AllowedDomains []string `json:"allowedDomains,omitempty"`

	// BlockClusterInternal blocks egress to private RFC1918 subnets and Carrier-Grade NAT (100.64.0.0/10).
	// Defaults according to isolationLevel.
	// +optional
	BlockClusterInternal *bool `json:"blockClusterInternal,omitempty"`

	// BlockMetadataService blocks egress to cloud instance metadata at 169.254.169.254/32.
	// Defaults according to isolationLevel.
	// +optional
	BlockMetadataService *bool `json:"blockMetadataService,omitempty"`

	// AllowAPIBroker permits access to the in-cluster provider credential broker.
	// +optional
	AllowAPIBroker bool `json:"allowAPIBroker,omitempty"`

	// AllowInternetWeb permits outbound HTTP and HTTPS traffic to ports 80 and 443.
	// Defaults according to isolationLevel.
	// +optional
	AllowInternetWeb *bool `json:"allowInternetWeb,omitempty"`

	// AllowDNS permits outbound DNS traffic to CoreDNS.
	// Defaults according to isolationLevel.
	// +optional
	AllowDNS *bool `json:"allowDNS,omitempty"`
}

// NetworkProfileStatus defines the observed state of NetworkProfile.
type NetworkProfileStatus struct {
	// SynthesizedNetworkPolicyName is the name of the managed NetworkPolicy.
	// +optional
	SynthesizedNetworkPolicyName string `json:"synthesizedNetworkPolicyName,omitempty"`

	// Conditions represent observations of current state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=np
// +kubebuilder:printcolumn:name="Isolation",type="string",JSONPath=".spec.isolationLevel"
// +kubebuilder:printcolumn:name="NetPol",type="string",JSONPath=".status.synthesizedNetworkPolicyName"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// NetworkProfile is the Schema for the networkprofiles API.
type NetworkProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NetworkProfileSpec   `json:"spec,omitempty"`
	Status NetworkProfileStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NetworkProfileList contains a list of NetworkProfile.
type NetworkProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NetworkProfile `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NetworkProfile{}, &NetworkProfileList{})
}
