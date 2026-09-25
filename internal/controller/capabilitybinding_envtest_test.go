// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// These specs run against a real API server, so they check what admission
// actually accepts on the two new catalog types — the content the projection
// controller later copies into customer projects.
var _ = Describe("ServiceAgent and ServiceAgentConfiguration", func() {
	// phase is a required, non-pointer field, so the Draft default never fires
	// — the same as every other catalog type. An unset phase is rejected rather
	// than quietly becoming Published.
	It("rejects an agent with no phase", func() {
		agent := &servicesv1alpha1.ServiceAgent{
			ObjectMeta: metav1.ObjectMeta{Name: "envtest-no-phase-agent"},
			Spec: servicesv1alpha1.ServiceAgentSpec{
				ServiceRef:  servicesv1alpha1.ServiceRef{Name: "envtest-service"},
				DisplayName: "Envtest agent",
			},
		}
		Expect(k8sClient.Create(ctx, agent)).NotTo(Succeed())
	})

	It("refuses to re-home an agent to another service", func() {
		agent := &servicesv1alpha1.ServiceAgent{
			ObjectMeta: metav1.ObjectMeta{Name: "envtest-immutable-agent"},
			Spec: servicesv1alpha1.ServiceAgentSpec{
				ServiceRef:  servicesv1alpha1.ServiceRef{Name: "envtest-service"},
				Phase:       servicesv1alpha1.PhasePublished,
				DisplayName: "Envtest agent",
			},
		}
		Expect(k8sClient.Create(ctx, agent)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, agent) })

		agent.Spec.ServiceRef.Name = "some-other-service"
		Expect(k8sClient.Update(ctx, agent)).NotTo(Succeed())
	})

	It("requires a version, since a binding with none cannot be traced back", func() {
		sac := &servicesv1alpha1.ServiceAgentConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: "envtest-no-version"},
			Spec: servicesv1alpha1.ServiceAgentConfigurationSpec{
				ServiceAgentRef: servicesv1alpha1.ServiceAgentReference{Name: "envtest-immutable-agent"},
				Phase:           servicesv1alpha1.PhasePublished,
			},
		}
		Expect(k8sClient.Create(ctx, sac)).NotTo(Succeed())
	})

	It("stores skills and reportingProject as written", func() {
		sac := &servicesv1alpha1.ServiceAgentConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: "envtest-full-config"},
			Spec: servicesv1alpha1.ServiceAgentConfigurationSpec{
				ServiceAgentRef:  servicesv1alpha1.ServiceAgentReference{Name: "envtest-immutable-agent"},
				Phase:            servicesv1alpha1.PhasePublished,
				Version:          "v1",
				ReportingProject: "datum-cloud",
				Skills: []servicesv1alpha1.AgentSkill{{
					Name:        "workload-not-available",
					Description: "Triage a Workload that is not available",
					Source:      "http://compute-mcp:8080/runbooks/workload-not-available.md",
				}},
				Tools: &servicesv1alpha1.AgentTools{
					MCPServers: []servicesv1alpha1.AgentMCPServer{{
						Name:     "compute",
						Endpoint: "http://compute-mcp:8080/mcp",
						ToolSelector: &servicesv1alpha1.AgentToolSelector{
							Include: []string{"compute_workloads_list"},
						},
					}},
				},
				Authority: &servicesv1alpha1.AgentAuthority{
					Reads: []servicesv1alpha1.AgentReadAuthority{{
						GVK: servicesv1alpha1.GVKRef{Group: "compute.datumapis.com", Kind: "*"},
					}},
					MaxTaskDurationSeconds: ptr.To(int64(60)),
				},
			},
		}
		Expect(k8sClient.Create(ctx, sac)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, sac) })

		fetched := &servicesv1alpha1.ServiceAgentConfiguration{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: sac.Name}, fetched)).To(Succeed())
		Expect(fetched.Spec.Skills).To(HaveLen(1))
		Expect(fetched.Spec.Skills[0].Source).To(Equal(sac.Spec.Skills[0].Source))
		Expect(fetched.Spec.ReportingProject).To(Equal("datum-cloud"))
		Expect(fetched.Spec.Tools.MCPServers[0].ToolSelector.Include).To(ConsistOf("compute_workloads_list"))
		Expect(*fetched.Spec.Authority.MaxTaskDurationSeconds).To(Equal(int64(60)))
	})

	It("rejects a skill with no description, which nothing could ever select", func() {
		sac := &servicesv1alpha1.ServiceAgentConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: "envtest-bad-skill"},
			Spec: servicesv1alpha1.ServiceAgentConfigurationSpec{
				ServiceAgentRef: servicesv1alpha1.ServiceAgentReference{Name: "envtest-immutable-agent"},
				Phase:           servicesv1alpha1.PhasePublished,
				Version:         "v1",
				Skills: []servicesv1alpha1.AgentSkill{{
					Name:   "no-description",
					Source: "http://compute-mcp:8080/runbooks/x.md",
				}},
			},
		}
		Expect(k8sClient.Create(ctx, sac)).NotTo(Succeed())
	})
})
