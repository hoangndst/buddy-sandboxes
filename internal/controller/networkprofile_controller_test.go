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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
)

var _ = Describe("NetworkProfile Controller", func() {
	const namespace = "default"

	Context("When reconciling a NetworkProfile", func() {
		It("should synthesize a shared NetworkPolicy with gateway egress when allowedDomains is set", func() {
			profileName := "test-gateway-netprofile"
			profileKey := types.NamespacedName{Name: profileName, Namespace: namespace}
			netpolKey := types.NamespacedName{Name: "np-" + profileName, Namespace: namespace}

			profile := &sandboxesv1alpha1.NetworkProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      profileName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.NetworkProfileSpec{
					IsolationLevel: sandboxesv1alpha1.IsolationHardPerimeter,
					AllowedDomains: []string{"github.com", "*.openai.com"},
				},
			}
			Expect(k8sClient.Create(ctx, profile)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, profile) }()

			reconciler := &NetworkProfileReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: profileKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())

			// Verify synthesized NetworkPolicy
			var netpol networkingv1.NetworkPolicy
			Expect(k8sClient.Get(ctx, netpolKey, &netpol)).To(Succeed())
			Expect(netpol.Spec.PodSelector.MatchLabels[networkProfileLabelKey]).To(Equal(profileName))
			Expect(netpol.OwnerReferences).NotTo(BeEmpty())
			Expect(netpol.OwnerReferences[0].Name).To(Equal(profileName))

			// Verify status
			var updatedProfile sandboxesv1alpha1.NetworkProfile
			Expect(k8sClient.Get(ctx, profileKey, &updatedProfile)).To(Succeed())
			Expect(updatedProfile.Status.SynthesizedNetworkPolicyName).To(Equal("np-" + profileName))
			Expect(updatedProfile.Status.Conditions).NotTo(BeEmpty())
			Expect(updatedProfile.Status.Conditions[0].Type).To(Equal("Ready"))
			Expect(string(updatedProfile.Status.Conditions[0].Status)).To(Equal(string(metav1.ConditionTrue)))
		})

		It("should synthesize direct web egress with CIDR exclusions when allowedDomains is empty", func() {
			profileName := "test-direct-netprofile"
			profileKey := types.NamespacedName{Name: profileName, Namespace: namespace}
			netpolKey := types.NamespacedName{Name: "np-" + profileName, Namespace: namespace}

			boolTrue := true
			profile := &sandboxesv1alpha1.NetworkProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      profileName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.NetworkProfileSpec{
					AllowInternetWeb:     &boolTrue,
					BlockClusterInternal: &boolTrue,
					BlockMetadataService: &boolTrue,
				},
			}
			Expect(k8sClient.Create(ctx, profile)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, profile) }()

			reconciler := &NetworkProfileReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: profileKey})
			Expect(err).NotTo(HaveOccurred())

			var netpol networkingv1.NetworkPolicy
			Expect(k8sClient.Get(ctx, netpolKey, &netpol)).To(Succeed())
			Expect(netpol.Spec.Egress).NotTo(BeEmpty())
		})

		It("should synthesize empty egress rules for DefaultDenyAll preset without overrides", func() {
			profileName := "test-denyall-netprofile"
			profileKey := types.NamespacedName{Name: profileName, Namespace: namespace}
			netpolKey := types.NamespacedName{Name: "np-" + profileName, Namespace: namespace}

			profile := &sandboxesv1alpha1.NetworkProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      profileName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.NetworkProfileSpec{
					IsolationLevel: sandboxesv1alpha1.IsolationDefaultDenyAll,
				},
			}
			Expect(k8sClient.Create(ctx, profile)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, profile) }()

			reconciler := &NetworkProfileReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: profileKey})
			Expect(err).NotTo(HaveOccurred())

			var netpol networkingv1.NetworkPolicy
			Expect(k8sClient.Get(ctx, netpolKey, &netpol)).To(Succeed())
			Expect(netpol.Spec.Egress).To(BeEmpty())
			Expect(netpol.Spec.Ingress).To(BeEmpty())
		})

		It("should permit DNS when explicitly overridden on DefaultDenyAll preset", func() {
			profileName := "test-denyall-override-dns"
			profileKey := types.NamespacedName{Name: profileName, Namespace: namespace}
			netpolKey := types.NamespacedName{Name: "np-" + profileName, Namespace: namespace}

			boolTrue := true
			profile := &sandboxesv1alpha1.NetworkProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      profileName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.NetworkProfileSpec{
					IsolationLevel: sandboxesv1alpha1.IsolationDefaultDenyAll,
					AllowDNS:       &boolTrue,
				},
			}
			Expect(k8sClient.Create(ctx, profile)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, profile) }()

			reconciler := &NetworkProfileReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: profileKey})
			Expect(err).NotTo(HaveOccurred())

			var netpol networkingv1.NetworkPolicy
			Expect(k8sClient.Get(ctx, netpolKey, &netpol)).To(Succeed())
			Expect(netpol.Spec.Egress).To(HaveLen(1))
			// Only kube-dns egress rule is present
			Expect(netpol.Spec.Egress[0].To).To(HaveLen(1))
			Expect(netpol.Spec.Egress[0].To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]).To(Equal("kube-system"))
		})

		It("should resolve DefaultDenyAll network settings accurately in adapter", func() {
			profileName := "test-adapter-denyall"
			profileKey := types.NamespacedName{Name: profileName, Namespace: namespace}

			profile := &sandboxesv1alpha1.NetworkProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      profileName,
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.NetworkProfileSpec{
					IsolationLevel: sandboxesv1alpha1.IsolationDefaultDenyAll,
				},
			}
			Expect(k8sClient.Create(ctx, profile)).To(Succeed())
			defer func() { _ = k8sClient.Delete(ctx, profile) }()

			npReconciler := &NetworkProfileReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			_, err := npReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: profileKey})
			Expect(err).NotTo(HaveOccurred())

			sbxReconciler := &SandboxReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			sbx := &sandboxesv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbx-adapter",
					Namespace: namespace,
				},
				Spec: sandboxesv1alpha1.SandboxSpec{
					NetworkProfileRef: &corev1.LocalObjectReference{
						Name: profileName,
					},
				},
			}

			resolved, err := sbxReconciler.resolveNetwork(ctx, sbx)
			Expect(err).NotTo(HaveOccurred())
			Expect(resolved.AllowInternetWeb).To(BeFalse())
			Expect(resolved.AllowDNS).To(BeFalse())
			Expect(resolved.BlockClusterInternal).To(BeTrue())
			Expect(resolved.BlockMetadataService).To(BeTrue())
			Expect(resolved.PolicyName).To(Equal("np-" + profileName))
		})
	})
})
