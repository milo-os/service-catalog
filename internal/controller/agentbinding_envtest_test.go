// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The AgentBinding envtest specs exercise the projection reconciler
// against a real kube-apiserver (CRDs, CEL rules, status subresources)
// in single-cluster mode: the same cluster serves as root and consumer
// project, mirroring how the local-slice harness engages envtest. The
// reconciler is driven directly, like the fake-manager unit tests,
// because envtest runs no kube-controller-manager (which is also why
// binding cleanup must not rely on owner-reference garbage collection).
var _ = Describe("AgentBindingReconciler", func() {
	const (
		consumerProject = "envtest-consumer"
		svcSlug         = "streaming-streamco-example"
		svcName         = "streaming.streamco.example"
		agentName       = "streamco-streaming-agent"
	)

	var created []client.Object

	// mustCreate creates the object and registers it for cleanup.
	mustCreate := func(obj client.Object) {
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		created = append(created, obj)
	}

	reconcileOnce := func() {
		mgr := newTestManager()
		mgr.add(consumerProject, k8sClient)
		r := &AgentBindingReconciler{
			rootClient: k8sClient,
			Manager:    mgr,
			Scheme:     scheme.Scheme,
		}
		req := mcreconcile.Request{
			Request:     ctrl.Request{NamespacedName: types.NamespacedName{Name: svcSlug}},
			ClusterName: consumerProject,
		}
		_, err := r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())
	}

	newEnvService := func() *servicesv1alpha1.Service {
		return &servicesv1alpha1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: svcSlug},
			Spec: servicesv1alpha1.ServiceSpec{
				ServiceName: svcName,
				Phase:       servicesv1alpha1.PhasePublished,
				DisplayName: "StreamCo Streaming",
				Owner: servicesv1alpha1.ServiceOwner{
					ProducerProjectRef: servicesv1alpha1.ProducerProjectReference{Name: "streamco-platform"},
				},
			},
		}
	}

	newEnvAgent := func() *servicesv1alpha1.ServiceAgent {
		return &servicesv1alpha1.ServiceAgent{
			ObjectMeta: metav1.ObjectMeta{Name: agentName},
			Spec: servicesv1alpha1.ServiceAgentSpec{
				ServiceRef:  servicesv1alpha1.ServiceRef{Name: svcSlug},
				Phase:       servicesv1alpha1.PhasePublished,
				DisplayName: "StreamCo Streaming Agent",
			},
		}
	}

	newEnvConfig := func(name, version string) *servicesv1alpha1.ServiceAgentConfiguration {
		return &servicesv1alpha1.ServiceAgentConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: servicesv1alpha1.ServiceAgentConfigurationSpec{
				ServiceAgentRef: servicesv1alpha1.ServiceAgentReference{Name: agentName},
				Version:         version,
				Phase:           servicesv1alpha1.PhasePublished,
				Knowledge: &servicesv1alpha1.AgentKnowledge{
					Sources: []servicesv1alpha1.AgentKnowledgeSource{
						{
							Type:  servicesv1alpha1.AgentKnowledgeSourceLLMDocs,
							Title: "StreamCo service overview",
							URL:   "http://127.0.0.1:7810/llms-full.txt",
						},
						{
							Type:  servicesv1alpha1.AgentKnowledgeSourceRunbook,
							Title: "Diagnosing stream lag",
							URL:   "http://127.0.0.1:7810/runbooks/lag.md",
						},
					},
					Concepts: []servicesv1alpha1.AgentConcept{{
						GVK:     servicesv1alpha1.GVKRef{Group: svcName, Kind: "Stream"},
						Summary: "A managed event stream.",
					}},
				},
				Tools: &servicesv1alpha1.AgentTools{
					MCPServers: []servicesv1alpha1.AgentMCPServer{{
						Name:     "streamco",
						Endpoint: "http://127.0.0.1:7810/mcp",
						ToolSelector: &servicesv1alpha1.AgentToolSelector{
							Include: []string{"streams_list", "streams_get", "pipeline_diagnose"},
						},
					}},
				},
				Authority: &servicesv1alpha1.AgentAuthority{
					Reads: []servicesv1alpha1.AgentReadAuthority{{
						GVK: servicesv1alpha1.GVKRef{Group: svcName, Kind: "*"},
					}},
					MaxTaskDurationSeconds: 120,
				},
				Metrics: []servicesv1alpha1.MetricSpec{{
					Name: svcName + "/agent/tool-invocations",
					Kind: servicesv1alpha1.MetricKindDelta,
					Unit: "{invocation}",
				}},
			},
		}
	}

	// newEnvEntitlement creates the entitlement and marks it Active via
	// the status subresource (the entitlement reconciler is not running
	// in this suite).
	newEnvEntitlement := func() *servicesv1alpha1.ServiceEntitlement {
		ent := &servicesv1alpha1.ServiceEntitlement{
			ObjectMeta: metav1.ObjectMeta{Name: svcSlug},
			Spec: servicesv1alpha1.ServiceEntitlementSpec{
				ServiceRef: servicesv1alpha1.ServiceRef{Name: svcSlug},
			},
		}
		mustCreate(ent)
		ent.Status.Phase = servicesv1alpha1.EntitlementPhaseActive
		Expect(k8sClient.Status().Update(ctx, ent)).To(Succeed())
		return ent
	}

	getBinding := func() (*servicesv1alpha1.AgentBinding, error) {
		var b servicesv1alpha1.AgentBinding
		err := k8sClient.Get(ctx, types.NamespacedName{Name: agentName}, &b)
		return &b, err
	}

	setConfigPhase := func(name string, phase servicesv1alpha1.Phase) {
		var cfg servicesv1alpha1.ServiceAgentConfiguration
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, &cfg)).To(Succeed())
		cfg.Spec.Phase = phase
		Expect(k8sClient.Update(ctx, &cfg)).To(Succeed())
	}

	AfterEach(func() {
		// Bindings are cleaned up by label; everything else in reverse
		// creation order. Deletes are best-effort so a failed spec does
		// not cascade.
		Expect(k8sClient.DeleteAllOf(ctx, &servicesv1alpha1.AgentBinding{},
			client.MatchingLabels{labelManagedBy: labelManagedByValue})).To(Succeed())
		for i := len(created) - 1; i >= 0; i-- {
			err := k8sClient.Delete(ctx, created[i])
			if err != nil && !apierrors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred())
			}
		}
		created = nil
	})

	It("projects a denormalized binding when all gates open and prunes it when the entitlement is deleted", func() {
		mustCreate(newEnvService())
		mustCreate(newEnvAgent())
		mustCreate(newEnvConfig(agentName+"-v1", "v1"))
		ent := newEnvEntitlement()

		By("reconciling with all gates open")
		reconcileOnce()

		b, err := getBinding()
		Expect(err).NotTo(HaveOccurred())
		Expect(b.Spec.ServiceName).To(Equal(svcName))
		Expect(b.Spec.ServiceRef.Name).To(Equal(svcSlug))
		Expect(b.Spec.ServiceAgentRef.Name).To(Equal(agentName))
		Expect(b.Spec.ConfigurationVersion).To(Equal("v1"))
		Expect(b.Spec.Knowledge).NotTo(BeNil())
		Expect(b.Spec.Knowledge.Sources).To(HaveLen(2))
		Expect(b.Spec.Knowledge.Sources[0].URL).To(Equal("http://127.0.0.1:7810/llms-full.txt"))
		Expect(b.Spec.Tools).NotTo(BeNil())
		Expect(b.Spec.Tools.MCPServers).To(HaveLen(1))
		Expect(b.Spec.Tools.MCPServers[0].Name).To(Equal("streamco"))
		Expect(b.Spec.Tools.MCPServers[0].Endpoint).To(Equal("http://127.0.0.1:7810/mcp"))
		Expect(b.Spec.Tools.MCPServers[0].ToolSelector.Include).To(ConsistOf(
			"streams_list", "streams_get", "pipeline_diagnose"))
		Expect(b.Spec.Authority).NotTo(BeNil())
		Expect(b.Spec.Authority.MaxTaskDurationSeconds).To(Equal(int64(120)))
		Expect(b.Spec.Authority.Reads).To(HaveLen(1))
		Expect(b.Spec.Authority.Reads[0].GVK.Kind).To(Equal("*"))

		By("carrying an owner reference to the entitlement and a Ready condition")
		Expect(ownedBy(b.OwnerReferences, ent.UID)).To(BeTrue())
		ready := apimeta.FindStatusCondition(b.Status.Conditions, ConditionTypeReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		Expect(ready.Reason).To(Equal(reasonAllAgentGatesOpen))

		By("pruning the binding after the entitlement is deleted (no GC in envtest)")
		Expect(k8sClient.Delete(ctx, ent)).To(Succeed())
		reconcileOnce()
		_, err = getBinding()
		Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"binding should be actively pruned once the entitlement is gone")
	})

	It("selects the latest Published configuration and follows phase changes until the gate closes", func() {
		mustCreate(newEnvService())
		mustCreate(newEnvAgent())
		mustCreate(newEnvConfig(agentName+"-v1", "v1"))
		mustCreate(newEnvConfig(agentName+"-v2", "v2"))
		newEnvEntitlement()

		By("selecting v2 (latest Published: newest creation, name tie-break)")
		reconcileOnce()
		b, err := getBinding()
		Expect(err).NotTo(HaveOccurred())
		Expect(b.Spec.ConfigurationVersion).To(Equal("v2"))

		By("falling back to v1 when v2 leaves the Published phase")
		setConfigPhase(agentName+"-v2", servicesv1alpha1.PhaseDeprecated)
		reconcileOnce()
		b, err = getBinding()
		Expect(err).NotTo(HaveOccurred())
		Expect(b.Spec.ConfigurationVersion).To(Equal("v1"))

		By("pruning the binding when the last Published configuration regresses out of Published")
		setConfigPhase(agentName+"-v1", servicesv1alpha1.PhaseDeprecated)
		reconcileOnce()
		_, err = getBinding()
		Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"binding should be pruned once no Published configuration remains")
	})

	It("enforces ServiceAgent serviceRef immutability via the CRD CEL rule", func() {
		mustCreate(newEnvService())
		agent := newEnvAgent()
		mustCreate(agent)

		agent.Spec.ServiceRef.Name = "some-other-service"
		err := k8sClient.Update(ctx, agent)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("serviceRef is immutable"))
	})
})
