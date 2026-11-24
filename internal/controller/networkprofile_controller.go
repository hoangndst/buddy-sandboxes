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

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
)

const (
	networkProfileLabelKey = "sandboxes.buddy.io/network-profile"
)

// NetworkProfileReconciler reconciles a NetworkProfile object.
type NetworkProfileReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=sandboxes.buddy.io,resources=networkprofiles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sandboxes.buddy.io,resources=networkprofiles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sandboxes.buddy.io,resources=networkprofiles/finalizers,verbs=update
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

func (r *NetworkProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var profile sandboxesv1alpha1.NetworkProfile
	if err := r.Get(ctx, req.NamespacedName, &profile); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch NetworkProfile")
		return ctrl.Result{}, err
	}

	allowInternet := true
	if profile.Spec.IsolationLevel == sandboxesv1alpha1.IsolationDefaultDenyAll {
		allowInternet = false
	}
	if profile.Spec.AllowInternetWeb != nil {
		allowInternet = *profile.Spec.AllowInternetWeb
	}

	if !allowInternet && len(profile.Spec.AllowedDomains) > 0 {
		meta.SetStatusCondition(&profile.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "ConflictingConfiguration",
			Message:            "allowedDomains cannot be configured when allowInternetWeb is false or under DefaultDenyAll",
			LastTransitionTime: metav1.Now(),
		})
		profile.Status.SynthesizedNetworkPolicyName = ""
		return ctrl.Result{}, r.Status().Update(ctx, &profile)
	}

	netpolName := fmt.Sprintf("np-%s", profile.Name)
	desiredNetPol := r.buildNetworkPolicy(&profile, netpolName)

	if err := ctrl.SetControllerReference(&profile, desiredNetPol, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to set controller reference on NetworkPolicy: %w", err)
	}

	var existingNetPol networkingv1.NetworkPolicy
	err := r.Get(ctx, client.ObjectKey{Namespace: profile.Namespace, Name: netpolName}, &existingNetPol)
	if err != nil && errors.IsNotFound(err) {
		if createErr := r.Create(ctx, desiredNetPol); createErr != nil {
			return ctrl.Result{}, fmt.Errorf("failed to create NetworkPolicy: %w", createErr)
		}
	} else if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to inspect existing NetworkPolicy: %w", err)
	} else {
		existingNetPol.Spec = desiredNetPol.Spec
		existingNetPol.OwnerReferences = desiredNetPol.OwnerReferences
		if updateErr := r.Update(ctx, &existingNetPol); updateErr != nil {
			return ctrl.Result{}, fmt.Errorf("failed to update NetworkPolicy: %w", updateErr)
		}
	}

	profile.Status.SynthesizedNetworkPolicyName = netpolName
	meta.SetStatusCondition(&profile.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "NetworkPolicyReady",
		Message:            "Shared NetworkPolicy successfully synthesized and active",
		LastTransitionTime: metav1.Now(),
	})

	if statusErr := r.Status().Update(ctx, &profile); statusErr != nil {
		return ctrl.Result{}, statusErr
	}

	return ctrl.Result{}, nil
}

func (r *NetworkProfileReconciler) buildNetworkPolicy(profile *sandboxesv1alpha1.NetworkProfile, name string) *networkingv1.NetworkPolicy {
	spec := profile.Spec
	allowInternet := true
	allowDNS := true
	blockCluster := true
	blockMetadata := true

	if spec.IsolationLevel == sandboxesv1alpha1.IsolationDefaultDenyAll {
		allowInternet = false
		allowDNS = false
	}

	if spec.AllowInternetWeb != nil {
		allowInternet = *spec.AllowInternetWeb
	}
	if spec.AllowDNS != nil {
		allowDNS = *spec.AllowDNS
	}
	if spec.BlockClusterInternal != nil {
		blockCluster = *spec.BlockClusterInternal
	}
	if spec.BlockMetadataService != nil {
		blockMetadata = *spec.BlockMetadataService
	}

	egressRules := []networkingv1.NetworkPolicyEgressRule{}

	if allowDNS {
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

	if allowInternet && len(spec.AllowedDomains) > 0 {
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
	} else if allowInternet {
		port80 := intstr.FromInt32(80)
		port443 := intstr.FromInt32(443)
		tcpProto := corev1.ProtocolTCP

		exceptCIDRs := []string{}
		if blockMetadata {
			exceptCIDRs = append(exceptCIDRs, "169.254.169.254/32")
		}
		if blockCluster {
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

	if spec.AllowAPIBroker {
		brokerPort := intstr.FromInt32(8080)
		tcpProto := corev1.ProtocolTCP
		egressRules = append(egressRules, networkingv1.NetworkPolicyEgressRule{
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcpProto, Port: &brokerPort}},
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
					"kubernetes.io/metadata.name": "buddy-sandboxes-system",
				}},
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
					"app.kubernetes.io/name": "buddy-api",
				}},
			}},
		})
	}

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: profile.Namespace,
			Labels: map[string]string{
				labelManagedByKey: managedByValue,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					networkProfileLabelKey: profile.Name,
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
}

// SetupWithManager sets up the controller with the Manager.
func (r *NetworkProfileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sandboxesv1alpha1.NetworkProfile{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Complete(r)
}
