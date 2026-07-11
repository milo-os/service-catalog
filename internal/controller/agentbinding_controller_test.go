// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const (
	abConsumerProject = "consumer-proj"
	abEntitlement     = "streaming"
	abServiceSlug     = "streaming"
	abServiceName     = "streaming.streamco.example"
	abAgentName       = "streamco-streaming-agent"
	abEntitlementUID  = types.UID("ent-streaming-uid")
)

// newAgentBindingRootClient builds the root-cluster client. Service,
// ServiceAgent, and ServiceAgentConfiguration are read-only here, so no
// status subresources are registered — that preserves any status seeded
// via WithObjects, mirroring the LocationBinding test harness.
func newAgentBindingRootClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(objs...).
		Build()
}

// newAgentBindingConsumerClient builds a project VCP client with
// AgentBinding status-subresource support. ServiceEntitlement is left
// out of the subresource set so the seeded status.phase survives object
// creation.
func newAgentBindingConsumerClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(testScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&servicesv1alpha1.AgentBinding{}).
		Build()
}

func newActiveAgentEntitlement() *servicesv1alpha1.ServiceEntitlement {
	return &servicesv1alpha1.ServiceEntitlement{
		ObjectMeta: metav1.ObjectMeta{Name: abEntitlement, UID: abEntitlementUID},
		Spec: servicesv1alpha1.ServiceEntitlementSpec{
			ServiceRef: servicesv1alpha1.ServiceRef{Name: abServiceSlug},
		},
		Status: servicesv1alpha1.ServiceEntitlementStatus{
			Phase: servicesv1alpha1.EntitlementPhaseActive,
		},
	}
}

func newStreamingService() *servicesv1alpha1.Service {
	return newPublishedService(abServiceSlug, abServiceName, testProviderProject, "")
}

func newPublishedAgent() *servicesv1alpha1.ServiceAgent {
	return newTestServiceAgent(abAgentName, abServiceSlug, servicesv1alpha1.PhasePublished)
}

// newProjectableConfig builds a Published configuration carrying the
// full projectable surface (knowledge, tools, authority) so tests can
// assert the denormalized copy.
func newProjectableConfig(name, version string, phase servicesv1alpha1.Phase, created time.Time) *servicesv1alpha1.ServiceAgentConfiguration {
	return &servicesv1alpha1.ServiceAgentConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: servicesv1alpha1.ServiceAgentConfigurationSpec{
			ServiceAgentRef: servicesv1alpha1.ServiceAgentReference{Name: abAgentName},
			Version:         version,
			Phase:           phase,
			Knowledge: &servicesv1alpha1.AgentKnowledge{
				Sources: []servicesv1alpha1.AgentKnowledgeSource{{
					Type:  servicesv1alpha1.AgentKnowledgeSourceLLMDocs,
					Title: "StreamCo service overview",
					URL:   "http://127.0.0.1:7810/llms-full.txt",
				}},
				Concepts: []servicesv1alpha1.AgentConcept{{
					GVK:     servicesv1alpha1.GVKRef{Group: abServiceName, Kind: "Stream"},
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
					GVK: servicesv1alpha1.GVKRef{Group: abServiceName, Kind: "*"},
				}},
				MaxTaskDurationSeconds: 120,
			},
		},
	}
}

// existingAgentBinding seeds a binding the reconciler would have
// projected earlier, for prune-path tests.
func existingAgentBinding(entitlementName string, ownerUID types.UID) *servicesv1alpha1.AgentBinding {
	return &servicesv1alpha1.AgentBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: abAgentName,
			Labels: map[string]string{
				labelManagedBy:           labelManagedByValue,
				labelBindingServiceName:  abServiceName,
				labelBindingServiceAgent: abAgentName,
				labelBindingEntitlement:  entitlementName,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         servicesv1alpha1.GroupVersion.String(),
				Kind:               "ServiceEntitlement",
				Name:               entitlementName,
				UID:                ownerUID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: servicesv1alpha1.AgentBindingSpec{
			ServiceRef:      servicesv1alpha1.ServiceRef{Name: abServiceSlug},
			ServiceName:     abServiceName,
			ServiceAgentRef: servicesv1alpha1.ServiceAgentReference{Name: abAgentName},
		},
	}
}

// reconcileAgentBindings runs one reconcile pass for the
// consumer-project entitlement.
func reconcileAgentBindings(t *testing.T, rootClient, consumerClient client.Client) (ctrl.Result, error) {
	t.Helper()
	mgr := newTestManager()
	mgr.add(abConsumerProject, consumerClient)
	r := &AgentBindingReconciler{
		rootClient: rootClient,
		Manager:    mgr,
		Scheme:     testScheme(),
	}
	req := mcreconcile.Request{
		Request:     ctrl.Request{NamespacedName: types.NamespacedName{Name: abEntitlement}},
		ClusterName: abConsumerProject,
	}
	return r.Reconcile(context.Background(), req)
}

func getAgentBinding(t *testing.T, c client.Client, name string) (*servicesv1alpha1.AgentBinding, bool) {
	t.Helper()
	var b servicesv1alpha1.AgentBinding
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &b); err != nil {
		return nil, false
	}
	return &b, true
}

func TestAgentBindingReconciler_AllGatesOpen(t *testing.T) {
	rootClient := newAgentBindingRootClient(
		newStreamingService(),
		newPublishedAgent(),
		newProjectableConfig(abAgentName+"-v1", "v1", servicesv1alpha1.PhasePublished, time.Now()),
	)
	consumerClient := newAgentBindingConsumerClient(newActiveAgentEntitlement())

	if _, err := reconcileAgentBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	b, ok := getAgentBinding(t, consumerClient, abAgentName)
	if !ok {
		t.Fatalf("expected AgentBinding %q to exist", abAgentName)
	}

	// The denormalized spec is projected from Service + configuration.
	if b.Spec.ServiceName != abServiceName {
		t.Errorf("spec.serviceName = %q, want %q", b.Spec.ServiceName, abServiceName)
	}
	if b.Spec.ServiceRef.Name != abServiceSlug {
		t.Errorf("spec.serviceRef.name = %q, want %q", b.Spec.ServiceRef.Name, abServiceSlug)
	}
	if b.Spec.ServiceAgentRef.Name != abAgentName {
		t.Errorf("spec.serviceAgentRef.name = %q, want %q", b.Spec.ServiceAgentRef.Name, abAgentName)
	}
	if b.Spec.ConfigurationVersion != "v1" {
		t.Errorf("spec.configurationVersion = %q, want v1", b.Spec.ConfigurationVersion)
	}
	if b.Spec.Knowledge == nil || len(b.Spec.Knowledge.Sources) != 1 ||
		b.Spec.Knowledge.Sources[0].URL != "http://127.0.0.1:7810/llms-full.txt" {
		t.Errorf("spec.knowledge not projected: %+v", b.Spec.Knowledge)
	}
	if b.Spec.Tools == nil || len(b.Spec.Tools.MCPServers) != 1 {
		t.Fatalf("spec.tools not projected: %+v", b.Spec.Tools)
	}
	srv := b.Spec.Tools.MCPServers[0]
	if srv.Name != "streamco" || srv.Endpoint != "http://127.0.0.1:7810/mcp" {
		t.Errorf("mcp server = %+v, want streamco @ http://127.0.0.1:7810/mcp", srv)
	}
	if srv.ToolSelector == nil || len(srv.ToolSelector.Include) != 3 {
		t.Errorf("toolSelector.include = %+v, want 3 tools", srv.ToolSelector)
	}
	if b.Spec.Authority == nil || b.Spec.Authority.MaxTaskDurationSeconds != 120 ||
		len(b.Spec.Authority.Reads) != 1 || b.Spec.Authority.Reads[0].GVK.Kind != "*" {
		t.Errorf("spec.authority not projected: %+v", b.Spec.Authority)
	}

	// Labels, owner reference, and Ready condition.
	if got := b.Labels[labelBindingServiceName]; got != abServiceName {
		t.Errorf("service-name label = %q, want %q", got, abServiceName)
	}
	if got := b.Labels[labelBindingEntitlement]; got != abEntitlement {
		t.Errorf("entitlement label = %q, want %q", got, abEntitlement)
	}
	if !ownedBy(b.OwnerReferences, abEntitlementUID) {
		t.Errorf("binding is not owned by the entitlement")
	}
	ready := apimeta.FindStatusCondition(b.Status.Conditions, ConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.Reason != reasonAllAgentGatesOpen {
		t.Errorf("Ready = %+v, want True/%s", ready, reasonAllAgentGatesOpen)
	}
}

func TestAgentBindingReconciler_LatestPublishedConfigurationWins(t *testing.T) {
	t0 := time.Now().Add(-2 * time.Hour)
	rootClient := newAgentBindingRootClient(
		newStreamingService(),
		newPublishedAgent(),
		newProjectableConfig(abAgentName+"-v1", "v1", servicesv1alpha1.PhasePublished, t0),
		newProjectableConfig(abAgentName+"-v2", "v2", servicesv1alpha1.PhasePublished, t0.Add(time.Hour)),
		// A newer Draft must NOT win over the older Published document.
		newProjectableConfig(abAgentName+"-v3", "v3", servicesv1alpha1.PhaseDraft, t0.Add(90*time.Minute)),
	)
	consumerClient := newAgentBindingConsumerClient(newActiveAgentEntitlement())

	if _, err := reconcileAgentBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	b, ok := getAgentBinding(t, consumerClient, abAgentName)
	if !ok {
		t.Fatalf("expected AgentBinding to exist")
	}
	if b.Spec.ConfigurationVersion != "v2" {
		t.Errorf("configurationVersion = %q, want v2 (latest Published)", b.Spec.ConfigurationVersion)
	}
}

func TestMoreRecentAgentConfiguration_TieBreaksOnName(t *testing.T) {
	now := time.Now()
	a := newProjectableConfig("cfg-a", "v1", servicesv1alpha1.PhasePublished, now)
	b := newProjectableConfig("cfg-b", "v2", servicesv1alpha1.PhasePublished, now)
	if moreRecentAgentConfiguration(a, b) {
		t.Errorf("expected cfg-b to win the name tie-break over cfg-a")
	}
	if !moreRecentAgentConfiguration(b, a) {
		t.Errorf("expected cfg-b to win the name tie-break over cfg-a")
	}
}

func TestAgentBindingReconciler_NoPublishedConfiguration(t *testing.T) {
	rootClient := newAgentBindingRootClient(
		newStreamingService(),
		newPublishedAgent(),
		newProjectableConfig(abAgentName+"-v1", "v1", servicesv1alpha1.PhaseDraft, time.Now()),
	)
	consumerClient := newAgentBindingConsumerClient(newActiveAgentEntitlement())

	if _, err := reconcileAgentBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := getAgentBinding(t, consumerClient, abAgentName); ok {
		t.Fatalf("expected no AgentBinding while no configuration is Published")
	}
}

func TestAgentBindingReconciler_AgentNotPublished(t *testing.T) {
	agent := newPublishedAgent()
	agent.Spec.Phase = servicesv1alpha1.PhaseDraft
	rootClient := newAgentBindingRootClient(
		newStreamingService(),
		agent,
		newProjectableConfig(abAgentName+"-v1", "v1", servicesv1alpha1.PhasePublished, time.Now()),
	)
	consumerClient := newAgentBindingConsumerClient(newActiveAgentEntitlement())

	if _, err := reconcileAgentBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := getAgentBinding(t, consumerClient, abAgentName); ok {
		t.Fatalf("expected no AgentBinding for a non-Published agent")
	}
}

// TestAgentBindingReconciler_ConfigurationGateCloses verifies that a
// binding projected from the only Published configuration is pruned
// once that configuration leaves the Published phase (Deprecated is the
// only regression the forward-only machine allows).
func TestAgentBindingReconciler_ConfigurationGateCloses(t *testing.T) {
	cfg := newProjectableConfig(abAgentName+"-v1", "v1", servicesv1alpha1.PhasePublished, time.Now())
	rootClient := newAgentBindingRootClient(newStreamingService(), newPublishedAgent(), cfg)
	consumerClient := newAgentBindingConsumerClient(newActiveAgentEntitlement())

	if _, err := reconcileAgentBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if _, ok := getAgentBinding(t, consumerClient, abAgentName); !ok {
		t.Fatalf("expected binding after first reconcile")
	}

	// Close gate 3 on the root cluster.
	var fresh servicesv1alpha1.ServiceAgentConfiguration
	if err := rootClient.Get(context.Background(), types.NamespacedName{Name: cfg.Name}, &fresh); err != nil {
		t.Fatalf("get config: %v", err)
	}
	fresh.Spec.Phase = servicesv1alpha1.PhaseDeprecated
	if err := rootClient.Update(context.Background(), &fresh); err != nil {
		t.Fatalf("update config: %v", err)
	}

	if _, err := reconcileAgentBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if _, ok := getAgentBinding(t, consumerClient, abAgentName); ok {
		t.Fatalf("expected binding to be pruned after the configuration left Published")
	}
}

func TestAgentBindingReconciler_EntitlementNotActive(t *testing.T) {
	rootClient := newAgentBindingRootClient(
		newStreamingService(),
		newPublishedAgent(),
		newProjectableConfig(abAgentName+"-v1", "v1", servicesv1alpha1.PhasePublished, time.Now()),
	)
	ent := newActiveAgentEntitlement()
	ent.Status.Phase = servicesv1alpha1.EntitlementPhasePendingApproval
	consumerClient := newAgentBindingConsumerClient(ent, existingAgentBinding(abEntitlement, abEntitlementUID))

	if _, err := reconcileAgentBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := getAgentBinding(t, consumerClient, abAgentName); ok {
		t.Fatalf("expected pre-existing binding to be pruned for a non-Active entitlement")
	}
}

func TestAgentBindingReconciler_EntitlementDeleting(t *testing.T) {
	rootClient := newAgentBindingRootClient(
		newStreamingService(),
		newPublishedAgent(),
		newProjectableConfig(abAgentName+"-v1", "v1", servicesv1alpha1.PhasePublished, time.Now()),
	)
	ent := newActiveAgentEntitlement()
	now := metav1.NewTime(time.Now())
	ent.DeletionTimestamp = &now
	ent.Finalizers = []string{serviceEntitlementFinalizer}
	consumerClient := newAgentBindingConsumerClient(ent, existingAgentBinding(abEntitlement, abEntitlementUID))

	if _, err := reconcileAgentBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := getAgentBinding(t, consumerClient, abAgentName); ok {
		t.Fatalf("expected binding to be pruned for a deleting entitlement")
	}
}

// TestAgentBindingReconciler_EntitlementGone verifies the reconciler
// actively prunes bindings for an entitlement that no longer exists —
// environments without the garbage collector (envtest) depend on this;
// the owner reference is only the production backstop.
func TestAgentBindingReconciler_EntitlementGone(t *testing.T) {
	rootClient := newAgentBindingRootClient(
		newStreamingService(),
		newPublishedAgent(),
		newProjectableConfig(abAgentName+"-v1", "v1", servicesv1alpha1.PhasePublished, time.Now()),
	)
	// Consumer cluster has the projected binding but no entitlement.
	consumerClient := newAgentBindingConsumerClient(existingAgentBinding(abEntitlement, abEntitlementUID))

	if _, err := reconcileAgentBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := getAgentBinding(t, consumerClient, abAgentName); ok {
		t.Fatalf("expected binding to be pruned after the entitlement disappeared")
	}
}

// TestAgentBindingReconciler_Idempotent locks in write-only-when-changed:
// a second reconcile over a settled binding must not bump its
// resourceVersion.
func TestAgentBindingReconciler_Idempotent(t *testing.T) {
	rootClient := newAgentBindingRootClient(
		newStreamingService(),
		newPublishedAgent(),
		newProjectableConfig(abAgentName+"-v1", "v1", servicesv1alpha1.PhasePublished, time.Now()),
	)
	consumerClient := newAgentBindingConsumerClient(newActiveAgentEntitlement())

	if _, err := reconcileAgentBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	first, ok := getAgentBinding(t, consumerClient, abAgentName)
	if !ok {
		t.Fatalf("expected binding after first reconcile")
	}

	if _, err := reconcileAgentBindings(t, rootClient, consumerClient); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	second, _ := getAgentBinding(t, consumerClient, abAgentName)
	if second.GetResourceVersion() != first.GetResourceVersion() {
		t.Errorf("second reconcile mutated binding (resourceVersion %s -> %s); expected a no-op",
			first.GetResourceVersion(), second.GetResourceVersion())
	}
}
