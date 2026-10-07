// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
	"go.miloapis.com/service-catalog/pkg/multicluster-runtime/e2esingle"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ServiceAgent visibility", func() {
	It("defaults an agent that says nothing about visibility to Required", func() {
		agent := &servicesv1alpha1.ServiceAgent{
			ObjectMeta: metav1.ObjectMeta{Name: "envtest-default-visibility"},
			Spec: servicesv1alpha1.ServiceAgentSpec{
				ServiceRef:  servicesv1alpha1.ServiceRef{Name: "envtest-service"},
				Phase:       servicesv1alpha1.PhaseDraft,
				DisplayName: "Envtest agent",
			},
		}
		Expect(k8sClient.Create(ctx, agent)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, agent) })

		fetched := &servicesv1alpha1.ServiceAgent{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: agent.Name}, fetched)).To(Succeed())
		Expect(fetched.Spec.Visibility.Entitlement).To(Equal(servicesv1alpha1.VisibilityEntitlementRequired))
		Expect(fetched.RequestsNoEntitlement()).To(BeFalse())
	})

	It("rejects an entitlement value other than Required or None", func() {
		agent := &servicesv1alpha1.ServiceAgent{
			ObjectMeta: metav1.ObjectMeta{Name: "envtest-bad-visibility"},
			Spec: servicesv1alpha1.ServiceAgentSpec{
				ServiceRef:  servicesv1alpha1.ServiceRef{Name: "envtest-service"},
				Phase:       servicesv1alpha1.PhaseDraft,
				DisplayName: "Envtest agent",
				Visibility:  servicesv1alpha1.ServiceAgentVisibility{Entitlement: "Optional"},
			},
		}
		Expect(k8sClient.Create(ctx, agent)).NotTo(Succeed())
	})

	// Runs the controllers on a real multicluster manager, so the watches are
	// wired as in production: the project here has no ServiceEntitlement, so
	// only the engagement trigger can project the agent, and only the
	// ServiceAgent watch on the root cluster can take it away again before the
	// periodic pass. A second agent asks for None without being allowlisted.
	It("projects an allowlisted None agent into a project with no entitlement and withdraws it on Required", func() {
		installCapabilityBindingCRD()

		svc := &servicesv1alpha1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "envtest-platform-svc"},
			Spec: servicesv1alpha1.ServiceSpec{
				ServiceName: "platform.miloapis.com",
				Phase:       servicesv1alpha1.PhasePublished,
				DisplayName: "Platform",
				Owner: servicesv1alpha1.ServiceOwner{
					ProducerProjectRef: servicesv1alpha1.ProducerProjectReference{Name: "platform"},
				},
			},
		}
		agent := &servicesv1alpha1.ServiceAgent{
			ObjectMeta: metav1.ObjectMeta{Name: "envtest-platform-agent"},
			Spec: servicesv1alpha1.ServiceAgentSpec{
				ServiceRef:  servicesv1alpha1.ServiceRef{Name: svc.Name},
				Phase:       servicesv1alpha1.PhasePublished,
				DisplayName: "Platform agent",
				Visibility: servicesv1alpha1.ServiceAgentVisibility{
					Entitlement: servicesv1alpha1.VisibilityEntitlementNone,
				},
			},
		}
		sac := &servicesv1alpha1.ServiceAgentConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: "envtest-platform-agent-v1"},
			Spec: servicesv1alpha1.ServiceAgentConfigurationSpec{
				ServiceAgentRef: servicesv1alpha1.ServiceAgentReference{Name: agent.Name},
				Phase:           servicesv1alpha1.PhasePublished,
				Version:         "v1",
			},
		}
		unlisted := agent.DeepCopy()
		unlisted.Name = "envtest-unlisted-agent"
		unlistedSAC := sac.DeepCopy()
		unlistedSAC.Name = "envtest-unlisted-agent-v1"
		unlistedSAC.Spec.ServiceAgentRef.Name = unlisted.Name
		for _, obj := range []client.Object{svc, agent, sac, unlisted, unlistedSAC} {
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, obj) })
		}

		local, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:  scheme.Scheme,
			Metrics: metricsserver.Options{BindAddress: "0"},
		})
		Expect(err).NotTo(HaveOccurred())
		mcMgr, err := mcmanager.WithMultiCluster(local, e2esingle.New(local))
		Expect(err).NotTo(HaveOccurred())
		allowed := map[string]struct{}{agent.Name: {}}
		r := &CapabilityBindingReconciler{Scheme: scheme.Scheme, EntitlementFreeAgents: allowed}
		Expect(r.SetupWithManager(mcMgr)).To(Succeed())
		// Reads must come from the cache that feeds the agent and
		// configuration watches, or a fanned-out pass can see the state from
		// before the event that triggered it.
		Expect(r.rootClient).To(BeIdenticalTo(mcMgr.GetLocalManager().GetClient()))
		Expect((&ServiceAgentReconciler{EntitlementFreeAgents: allowed}).SetupWithManager(local)).To(Succeed())

		mgrCtx, stop := context.WithCancel(ctx)
		DeferCleanup(stop)
		go func() {
			defer GinkgoRecover()
			Expect(mcMgr.Start(mgrCtx)).To(Succeed())
		}()

		bindingFor := func(name string) func() error {
			return func() error {
				u := &unstructured.Unstructured{}
				u.SetGroupVersionKind(capabilityBindingGVK)
				return k8sClient.Get(ctx, types.NamespacedName{Name: name}, u)
			}
		}
		binding := bindingFor(agent.Name)
		Eventually(binding, 30*time.Second, 200*time.Millisecond).Should(Succeed())

		waived := func(name string) func() metav1.ConditionStatus {
			return func() metav1.ConditionStatus {
				fetched := &servicesv1alpha1.ServiceAgent{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, fetched); err != nil {
					return ""
				}
				c := apimeta.FindStatusCondition(fetched.Status.Conditions, servicesv1alpha1.ConditionTypeEntitlementWaived)
				if c == nil {
					return ""
				}
				return c.Status
			}
		}
		Eventually(waived(agent.Name), 30*time.Second, 200*time.Millisecond).Should(Equal(metav1.ConditionTrue))
		Eventually(waived(unlisted.Name), 30*time.Second, 200*time.Millisecond).Should(Equal(metav1.ConditionFalse))
		Consistently(func() bool { return apierrors.IsNotFound(bindingFor(unlisted.Name)()) },
			2*time.Second, 200*time.Millisecond).Should(BeTrue(),
			"an agent that is not allowlisted should not reach a project with no entitlement")
		DeferCleanup(func() {
			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(capabilityBindingGVK)
			u.SetName(agent.Name)
			_ = k8sClient.Delete(ctx, u)
		})

		Eventually(func() error {
			fetched := &servicesv1alpha1.ServiceAgent{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: agent.Name}, fetched); err != nil {
				return err
			}
			fetched.Spec.Visibility.Entitlement = servicesv1alpha1.VisibilityEntitlementRequired
			return k8sClient.Update(ctx, fetched)
		}).Should(Succeed())

		Eventually(func() bool {
			return apierrors.IsNotFound(binding())
		}, 30*time.Second, 200*time.Millisecond).Should(BeTrue(),
			"setting Required should withdraw the agent from a project with no entitlement")
		Eventually(waived(agent.Name), 30*time.Second, 200*time.Millisecond).Should(BeEmpty(),
			"an agent asking for Required carries no EntitlementWaived condition")
	})
})

// installCapabilityBindingCRD registers a schemaless stand-in for the
// assistant's CapabilityBinding, which lives in another repo, so the
// controller has somewhere to write. Written untyped to keep the
// apiextensions API out of this module's direct dependencies.
func installCapabilityBindingCRD() {
	crd := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": "capabilitybindings.capabilities.assistant.miloapis.com"},
		"spec": map[string]any{
			"group": capabilityBindingGVK.Group,
			"scope": "Cluster",
			"names": map[string]any{
				"kind":     capabilityBindingGVK.Kind,
				"listKind": capabilityBindingGVK.Kind + "List",
				"plural":   "capabilitybindings",
				"singular": "capabilitybinding",
			},
			"versions": []any{map[string]any{
				"name":    capabilityBindingGVK.Version,
				"served":  true,
				"storage": true,
				"schema": map[string]any{"openAPIV3Schema": map[string]any{
					"type":                                 "object",
					"x-kubernetes-preserve-unknown-fields": true,
				}},
			}},
		},
	}}
	err := k8sClient.Create(ctx, crd)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}

	// Wait until the API serves the kind, using a fresh client so its
	// discovery is not stale.
	Eventually(func() error {
		c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
		if err != nil {
			return err
		}
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(capabilityBindingGVK.GroupVersion().WithKind(capabilityBindingGVK.Kind + "List"))
		return c.List(ctx, list)
	}, 30*time.Second, 200*time.Millisecond).Should(Succeed())
}
