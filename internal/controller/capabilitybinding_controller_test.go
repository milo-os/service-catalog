// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const (
	cbProject      = "consumer-proj"
	cbService      = "compute-miloapis-com"
	cbServiceName  = "compute.miloapis.com"
	cbEntitlement  = "compute-entitlement"
	cbAgent        = "compute-diagnostics-agent"
	cbConfigV1     = "compute-diagnostics-agent-v1"
	cbReportingPrj = "datum-cloud"
)

// capabilityScheme registers the services types plus the assistant's
// CapabilityBinding as unstructured, so the fake project client can serve the
// projection writes and the prune list.
func capabilityScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = servicesv1alpha1.AddToScheme(s)
	s.AddKnownTypeWithName(capabilityBindingGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(
		capabilityBindingGVK.GroupVersion().WithKind(capabilityBindingGVK.Kind+"List"),
		&unstructured.UnstructuredList{},
	)
	return s
}

func cbRootClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(capabilityScheme()).WithObjects(objs...).Build()
}

// cbConsumerClient builds a project client. ServiceEntitlement has no status
// subresource here, so the seeded status.phase survives creation.
func cbConsumerClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(capabilityScheme()).WithObjects(objs...).Build()
}

func cbEntitlementActive() *servicesv1alpha1.ServiceEntitlement {
	return &servicesv1alpha1.ServiceEntitlement{
		ObjectMeta: metav1.ObjectMeta{Name: cbEntitlement},
		Spec: servicesv1alpha1.ServiceEntitlementSpec{
			ServiceRef: servicesv1alpha1.ServiceRef{Name: cbService},
		},
		Status: servicesv1alpha1.ServiceEntitlementStatus{
			Phase: servicesv1alpha1.EntitlementPhaseActive,
		},
	}
}

func cbServiceObject() *servicesv1alpha1.Service {
	return &servicesv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: cbService},
		Spec: servicesv1alpha1.ServiceSpec{
			ServiceName: cbServiceName,
			Phase:       servicesv1alpha1.PhasePublished,
			DisplayName: "Compute",
		},
	}
}

func cbAgentObject(phase servicesv1alpha1.Phase) *servicesv1alpha1.ServiceAgent {
	return &servicesv1alpha1.ServiceAgent{
		ObjectMeta: metav1.ObjectMeta{Name: cbAgent},
		Spec: servicesv1alpha1.ServiceAgentSpec{
			ServiceRef:  servicesv1alpha1.ServiceRef{Name: cbService},
			Phase:       phase,
			DisplayName: "Compute diagnostics",
		},
	}
}

func cbConfigObject(name, version string, phase servicesv1alpha1.Phase) *servicesv1alpha1.ServiceAgentConfiguration {
	return &servicesv1alpha1.ServiceAgentConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: servicesv1alpha1.ServiceAgentConfigurationSpec{
			ServiceAgentRef:  servicesv1alpha1.ServiceAgentReference{Name: cbAgent},
			Phase:            phase,
			Version:          version,
			ReportingProject: cbReportingPrj,
			Knowledge: &servicesv1alpha1.AgentKnowledge{
				Sources: []servicesv1alpha1.AgentKnowledgeSource{{
					Type:  servicesv1alpha1.AgentKnowledgeSourceLLMDocs,
					Title: "Compute resource model",
					URL:   "http://compute-mcp:8080/llms-full.txt",
				}},
				Concepts: []servicesv1alpha1.AgentConcept{{
					GVK:     servicesv1alpha1.GVKRef{Group: "compute.datumapis.com", Kind: "Workload"},
					Summary: "A customer's declared compute intent.",
				}},
			},
			Tools: &servicesv1alpha1.AgentTools{
				MCPServers: []servicesv1alpha1.AgentMCPServer{{
					Name:     "compute",
					Endpoint: "http://compute-mcp:8080/mcp",
					ToolSelector: &servicesv1alpha1.AgentToolSelector{
						Include: []string{"compute_workloads_list", "compute_workload_diagnose"},
					},
					Mutating: []string{},
				}},
			},
			Skills: []servicesv1alpha1.AgentSkill{
				{
					Name:        "workload-not-available",
					Description: "Triage a Workload that is not available",
					Source:      "http://compute-mcp:8080/runbooks/workload-not-available.md",
				},
				{
					Name:        "quota-triage",
					Description: "Distinguish quota failures and act on each",
					Source:      "http://compute-mcp:8080/runbooks/quota-triage.md",
				},
			},
			Authority: &servicesv1alpha1.AgentAuthority{
				Reads: []servicesv1alpha1.AgentReadAuthority{{
					GVK: servicesv1alpha1.GVKRef{Group: "compute.datumapis.com", Kind: "*"},
				}},
				MaxTaskDurationSeconds: ptr.To(int64(60)),
			},
		},
	}
}

// existingCapabilityBinding is a binding this operator previously wrote, used
// to exercise the prune path.
func existingCapabilityBinding(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(capabilityBindingGVK)
	u.SetName(name)
	u.SetLabels(map[string]string{labelManagedBy: labelManagedByValue})
	_ = unstructured.SetNestedMap(u.Object, map[string]any{"serviceName": cbServiceName}, "spec")
	return u
}

func newCapabilityReconciler(rootClient, consumerClient client.Client) *CapabilityBindingReconciler {
	mgr := newTestManager()
	mgr.add(cbProject, consumerClient)
	return &CapabilityBindingReconciler{
		rootClient: rootClient,
		Manager:    mgr,
		Scheme:     capabilityScheme(),
	}
}

func cbRequest() mcreconcile.Request {
	return mcreconcile.Request{
		Request:     ctrlreconcile.Request{NamespacedName: types.NamespacedName{Name: locationBindingRequestName}},
		ClusterName: cbProject,
	}
}

func getCapabilityBinding(t *testing.T, c client.Client, name string) (*unstructured.Unstructured, bool) {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(capabilityBindingGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, u); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false
		}
		t.Fatalf("get CapabilityBinding %q: %v", name, err)
	}
	return u, true
}

func TestCapabilityBinding_AllGatesOpen(t *testing.T) {
	root := cbRootClient(
		cbServiceObject(),
		cbAgentObject(servicesv1alpha1.PhasePublished),
		cbConfigObject(cbConfigV1, "v1", servicesv1alpha1.PhasePublished),
	)
	consumer := cbConsumerClient(cbEntitlementActive())
	r := newCapabilityReconciler(root, consumer)

	res, err := r.Reconcile(context.Background(), cbRequest())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != capabilityBindingResyncInterval {
		t.Fatalf("RequeueAfter = %v, want %v", res.RequeueAfter, capabilityBindingResyncInterval)
	}

	u, ok := getCapabilityBinding(t, consumer, cbAgent)
	if !ok {
		t.Fatalf("expected a CapabilityBinding named %q", cbAgent)
	}

	// The project's control plane is the tenancy boundary; a namespace here
	// would put the object somewhere the assistant never looks.
	if ns := u.GetNamespace(); ns != "" {
		t.Errorf("namespace = %q, want empty", ns)
	}

	if got, _, _ := unstructured.NestedString(u.Object, "spec", "serviceRef", "name"); got != cbService {
		t.Errorf("spec.serviceRef.name = %q, want %q", got, cbService)
	}
	if got, _, _ := unstructured.NestedString(u.Object, "spec", "serviceName"); got != cbServiceName {
		t.Errorf("spec.serviceName = %q, want %q", got, cbServiceName)
	}
	if got, _, _ := unstructured.NestedString(u.Object, "spec", "serviceAgentRef", "name"); got != cbAgent {
		t.Errorf("spec.serviceAgentRef.name = %q, want %q", got, cbAgent)
	}
	if got, _, _ := unstructured.NestedString(u.Object, "spec", "configurationVersion"); got != "v1" {
		t.Errorf("spec.configurationVersion = %q, want v1", got)
	}
	// reportingProject is what makes capability-gap reporting exist at all.
	if got, _, _ := unstructured.NestedString(u.Object, "spec", "reportingProject"); got != cbReportingPrj {
		t.Errorf("spec.reportingProject = %q, want %q", got, cbReportingPrj)
	}

	skills, found, err := unstructured.NestedSlice(u.Object, "spec", "skills")
	if err != nil || !found {
		t.Fatalf("spec.skills missing: found=%v err=%v", found, err)
	}
	if len(skills) != 2 {
		t.Fatalf("spec.skills length = %d, want 2", len(skills))
	}
	first, _ := skills[0].(map[string]any)
	for _, key := range []string{"name", "description", "source"} {
		if v, ok := first[key].(string); !ok || v == "" {
			t.Errorf("spec.skills[0].%s missing", key)
		}
	}

	servers, _, _ := unstructured.NestedSlice(u.Object, "spec", "tools", "mcpServers")
	if len(servers) != 1 {
		t.Fatalf("spec.tools.mcpServers length = %d, want 1", len(servers))
	}
	server, _ := servers[0].(map[string]any)
	selector, _ := server["toolSelector"].(map[string]any)
	includeList, _ := selector["include"].([]any)
	if len(includeList) != 2 {
		t.Errorf("toolSelector.include length = %d, want 2", len(includeList))
	}

	sources, _, _ := unstructured.NestedSlice(u.Object, "spec", "knowledge", "sources")
	if len(sources) != 1 {
		t.Errorf("spec.knowledge.sources length = %d, want 1", len(sources))
	}

	// maxTaskDurationSeconds must survive as an integer, not a float.
	if got, found, err := unstructured.NestedInt64(u.Object, "spec", "authority", "maxTaskDurationSeconds"); err != nil || !found || got != 60 {
		t.Errorf("spec.authority.maxTaskDurationSeconds = %v (found=%v err=%v), want 60", got, found, err)
	}

	labels := u.GetLabels()
	if labels[labelManagedBy] != labelManagedByValue {
		t.Errorf("missing managed-by label: %v", labels)
	}
	if labels[labelCapabilityServiceAgent] != cbAgent {
		t.Errorf("service-agent label = %q, want %q", labels[labelCapabilityServiceAgent], cbAgent)
	}
}

func TestCapabilityBinding_AgentNotPublished(t *testing.T) {
	for _, phase := range []servicesv1alpha1.Phase{
		servicesv1alpha1.PhaseDraft,
		servicesv1alpha1.PhaseRetired,
	} {
		root := cbRootClient(
			cbServiceObject(),
			cbAgentObject(phase),
			cbConfigObject(cbConfigV1, "v1", servicesv1alpha1.PhasePublished),
		)
		consumer := cbConsumerClient(cbEntitlementActive())
		r := newCapabilityReconciler(root, consumer)

		if _, err := r.Reconcile(context.Background(), cbRequest()); err != nil {
			t.Fatalf("reconcile (%s): %v", phase, err)
		}
		if _, ok := getCapabilityBinding(t, consumer, cbAgent); ok {
			t.Errorf("agent phase %s should not project a binding", phase)
		}
	}
}

func TestCapabilityBinding_NoPublishedConfiguration(t *testing.T) {
	root := cbRootClient(
		cbServiceObject(),
		cbAgentObject(servicesv1alpha1.PhasePublished),
		cbConfigObject(cbConfigV1, "v1", servicesv1alpha1.PhaseDraft),
	)
	consumer := cbConsumerClient(cbEntitlementActive())
	r := newCapabilityReconciler(root, consumer)

	if _, err := r.Reconcile(context.Background(), cbRequest()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := getCapabilityBinding(t, consumer, cbAgent); ok {
		t.Error("a Draft configuration should not project a binding")
	}
}

// A blank version would leave the assistant unable to trace what a customer
// saw, and it rejects the object outright, so the gate stays closed.
func TestCapabilityBinding_ConfigurationWithoutVersionSkipped(t *testing.T) {
	root := cbRootClient(
		cbServiceObject(),
		cbAgentObject(servicesv1alpha1.PhasePublished),
		cbConfigObject(cbConfigV1, "", servicesv1alpha1.PhasePublished),
	)
	consumer := cbConsumerClient(cbEntitlementActive())
	r := newCapabilityReconciler(root, consumer)

	if _, err := r.Reconcile(context.Background(), cbRequest()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := getCapabilityBinding(t, consumer, cbAgent); ok {
		t.Error("a configuration with no version should not project a binding")
	}
}

func TestCapabilityBinding_LatestPublishedConfigurationWins(t *testing.T) {
	older := cbConfigObject("agent-v1", "v1", servicesv1alpha1.PhasePublished)
	older.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	newer := cbConfigObject("agent-v2", "v2", servicesv1alpha1.PhasePublished)
	newer.CreationTimestamp = metav1.NewTime(time.Now())

	root := cbRootClient(cbServiceObject(), cbAgentObject(servicesv1alpha1.PhasePublished), older, newer)
	consumer := cbConsumerClient(cbEntitlementActive())
	r := newCapabilityReconciler(root, consumer)

	if _, err := r.Reconcile(context.Background(), cbRequest()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	u, ok := getCapabilityBinding(t, consumer, cbAgent)
	if !ok {
		t.Fatal("expected a binding")
	}
	if got, _, _ := unstructured.NestedString(u.Object, "spec", "configurationVersion"); got != "v2" {
		t.Errorf("configurationVersion = %q, want v2", got)
	}
}

func TestCapabilityBinding_TiedTimestampsBreakOnName(t *testing.T) {
	stamp := metav1.NewTime(time.Now())
	a := cbConfigObject("agent-aaa", "va", servicesv1alpha1.PhasePublished)
	a.CreationTimestamp = stamp
	b := cbConfigObject("agent-zzz", "vz", servicesv1alpha1.PhasePublished)
	b.CreationTimestamp = stamp

	root := cbRootClient(cbServiceObject(), cbAgentObject(servicesv1alpha1.PhasePublished), a, b)
	consumer := cbConsumerClient(cbEntitlementActive())
	r := newCapabilityReconciler(root, consumer)

	if _, err := r.Reconcile(context.Background(), cbRequest()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	u, _ := getCapabilityBinding(t, consumer, cbAgent)
	if u == nil {
		t.Fatal("expected a binding")
	}
	if got, _, _ := unstructured.NestedString(u.Object, "spec", "configurationVersion"); got != "vz" {
		t.Errorf("configurationVersion = %q, want vz (higher name wins the tie)", got)
	}
}

// Entitlement withdrawn: the project's assistant must stop offering the
// service, so an existing binding is removed.
func TestCapabilityBinding_EntitlementNotActivePrunes(t *testing.T) {
	entitlement := cbEntitlementActive()
	entitlement.Status.Phase = servicesv1alpha1.EntitlementPhaseRejected

	root := cbRootClient(
		cbServiceObject(),
		cbAgentObject(servicesv1alpha1.PhasePublished),
		cbConfigObject(cbConfigV1, "v1", servicesv1alpha1.PhasePublished),
	)
	consumer := cbConsumerClient(entitlement, existingCapabilityBinding(cbAgent))
	r := newCapabilityReconciler(root, consumer)

	if _, err := r.Reconcile(context.Background(), cbRequest()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := getCapabilityBinding(t, consumer, cbAgent); ok {
		t.Error("a non-Active entitlement should leave no binding behind")
	}
}

func TestCapabilityBinding_NoEntitlementsPrunes(t *testing.T) {
	root := cbRootClient(
		cbServiceObject(),
		cbAgentObject(servicesv1alpha1.PhasePublished),
		cbConfigObject(cbConfigV1, "v1", servicesv1alpha1.PhasePublished),
	)
	consumer := cbConsumerClient(existingCapabilityBinding(cbAgent))
	r := newCapabilityReconciler(root, consumer)

	if _, err := r.Reconcile(context.Background(), cbRequest()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := getCapabilityBinding(t, consumer, cbAgent); ok {
		t.Error("a project with no entitlements should hold no bindings")
	}
}

// An agent pulled from publication takes its binding with it.
func TestCapabilityBinding_GateClosesPrunes(t *testing.T) {
	root := cbRootClient(
		cbServiceObject(),
		cbAgentObject(servicesv1alpha1.PhaseDeprecated),
		cbConfigObject(cbConfigV1, "v1", servicesv1alpha1.PhasePublished),
	)
	consumer := cbConsumerClient(cbEntitlementActive(), existingCapabilityBinding(cbAgent))
	r := newCapabilityReconciler(root, consumer)

	if _, err := r.Reconcile(context.Background(), cbRequest()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := getCapabilityBinding(t, consumer, cbAgent); ok {
		t.Error("unpublishing the agent should remove its binding")
	}
}

// A binding the operator never wrote (a dev or e2e fixture) has no managed-by
// label and must survive the sweep.
func TestCapabilityBinding_HandWrittenBindingSurvivesPrune(t *testing.T) {
	handWritten := &unstructured.Unstructured{}
	handWritten.SetGroupVersionKind(capabilityBindingGVK)
	handWritten.SetName("someone-elses-agent")
	_ = unstructured.SetNestedMap(handWritten.Object, map[string]any{"serviceName": "other.example"}, "spec")

	root := cbRootClient(cbServiceObject())
	consumer := cbConsumerClient(cbEntitlementActive(), handWritten)
	r := newCapabilityReconciler(root, consumer)

	if _, err := r.Reconcile(context.Background(), cbRequest()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := getCapabilityBinding(t, consumer, "someone-elses-agent"); !ok {
		t.Error("an unmanaged binding must not be pruned")
	}
}

// A hand-written binding under the agent's name converges onto the catalog's
// content rather than being duplicated alongside it.
func TestCapabilityBinding_ConvergesOntoSameName(t *testing.T) {
	handWritten := &unstructured.Unstructured{}
	handWritten.SetGroupVersionKind(capabilityBindingGVK)
	handWritten.SetName(cbAgent)
	_ = unstructured.SetNestedMap(handWritten.Object, map[string]any{
		"serviceName":          "stale.example",
		"configurationVersion": "hand-written",
	}, "spec")

	root := cbRootClient(
		cbServiceObject(),
		cbAgentObject(servicesv1alpha1.PhasePublished),
		cbConfigObject(cbConfigV1, "v1", servicesv1alpha1.PhasePublished),
	)
	consumer := cbConsumerClient(cbEntitlementActive(), handWritten)
	r := newCapabilityReconciler(root, consumer)

	if _, err := r.Reconcile(context.Background(), cbRequest()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(capabilityBindingGVK.GroupVersion().WithKind(capabilityBindingGVK.Kind + "List"))
	if err := consumer.List(context.Background(), &list); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("binding count = %d, want 1 (no duplicate)", len(list.Items))
	}
	if got, _, _ := unstructured.NestedString(list.Items[0].Object, "spec", "configurationVersion"); got != "v1" {
		t.Errorf("configurationVersion = %q, want v1", got)
	}
}

func TestCapabilityBinding_Idempotent(t *testing.T) {
	root := cbRootClient(
		cbServiceObject(),
		cbAgentObject(servicesv1alpha1.PhasePublished),
		cbConfigObject(cbConfigV1, "v1", servicesv1alpha1.PhasePublished),
	)
	consumer := cbConsumerClient(cbEntitlementActive())
	r := newCapabilityReconciler(root, consumer)

	if _, err := r.Reconcile(context.Background(), cbRequest()); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	first, ok := getCapabilityBinding(t, consumer, cbAgent)
	if !ok {
		t.Fatal("expected a binding")
	}
	if _, err := r.Reconcile(context.Background(), cbRequest()); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	second, _ := getCapabilityBinding(t, consumer, cbAgent)
	if second == nil {
		t.Fatal("expected a binding")
	}
	if first.GetResourceVersion() != second.GetResourceVersion() {
		t.Errorf("settled binding was rewritten: %s -> %s",
			first.GetResourceVersion(), second.GetResourceVersion())
	}
}

// A project whose control plane has no CapabilityBinding CRD is not an error:
// nothing there reads one.
func TestCapabilityBinding_KindNotInstalled(t *testing.T) {
	noMatch := &apimeta.NoKindMatchError{
		GroupKind:        schema.GroupKind{Group: capabilityBindingGVK.Group, Kind: capabilityBindingGVK.Kind},
		SearchedVersions: []string{capabilityBindingGVK.Version},
	}
	consumer := fake.NewClientBuilder().
		WithScheme(capabilityScheme()).
		WithObjects(cbEntitlementActive()).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if obj.GetObjectKind().GroupVersionKind() == capabilityBindingGVK {
					return noMatch
				}
				return c.Get(ctx, key, obj, opts...)
			},
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if list.GetObjectKind().GroupVersionKind().Kind == capabilityBindingGVK.Kind+"List" {
					return noMatch
				}
				return c.List(ctx, list, opts...)
			},
		}).
		Build()

	root := cbRootClient(
		cbServiceObject(),
		cbAgentObject(servicesv1alpha1.PhasePublished),
		cbConfigObject(cbConfigV1, "v1", servicesv1alpha1.PhasePublished),
	)
	r := newCapabilityReconciler(root, consumer)

	if _, err := r.Reconcile(context.Background(), cbRequest()); err != nil {
		t.Fatalf("a missing CapabilityBinding CRD should not fail the reconcile: %v", err)
	}
}

// An entitled service the catalog has not registered yet contributes nothing
// and is retried on the next pass, rather than failing the whole project.
func TestCapabilityBinding_MissingServiceSkipped(t *testing.T) {
	root := cbRootClient(
		cbAgentObject(servicesv1alpha1.PhasePublished),
		cbConfigObject(cbConfigV1, "v1", servicesv1alpha1.PhasePublished),
	)
	consumer := cbConsumerClient(cbEntitlementActive())
	r := newCapabilityReconciler(root, consumer)

	if _, err := r.Reconcile(context.Background(), cbRequest()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := getCapabilityBinding(t, consumer, cbAgent); ok {
		t.Error("no Service means nothing to project")
	}
}

func TestCapabilityBinding_RequiresClusterName(t *testing.T) {
	r := newCapabilityReconciler(cbRootClient(), cbConsumerClient())
	if _, err := r.Reconcile(context.Background(), mcreconcile.Request{}); err == nil {
		t.Error("expected an error when no cluster name is given")
	}
}

// An agent pointing at a catalog entry that does not exist can never reach a
// customer. It must not project, and it must be reported rather than skipped in
// silence — that silence is what once hid a one-word mistake behind an absence
// across every project.
func TestCapabilityBinding_AgentNamingUnknownServiceIsReportedOnce(t *testing.T) {
	agent := cbAgentObject(servicesv1alpha1.PhasePublished)
	agent.Spec.ServiceRef.Name = "not-in-the-catalog"

	root := cbRootClient(
		cbServiceObject(),
		agent,
		cbConfigObject(cbConfigV1, "v1", servicesv1alpha1.PhasePublished),
	)
	consumer := cbConsumerClient(cbEntitlementActive())
	r := newCapabilityReconciler(root, consumer)

	if _, err := r.Reconcile(context.Background(), cbRequest()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := getCapabilityBinding(t, consumer, cbAgent); ok {
		t.Fatalf("expected no CapabilityBinding for an agent naming an unknown service")
	}
	if _, warned := r.warnedAgents.Load(cbAgent); !warned {
		t.Errorf("expected the agent to be recorded as warned")
	}

	// Second pass: still no binding, and the agent stays recorded so the
	// complaint is not repeated on every project.
	if _, err := r.Reconcile(context.Background(), cbRequest()); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if _, warned := r.warnedAgents.Load(cbAgent); !warned {
		t.Errorf("expected the warned record to persist")
	}
}

// Once the reference is corrected the agent projects normally and the warning
// record is cleared, so a later regression is reported again.
func TestCapabilityBinding_WarningClearsWhenServiceResolves(t *testing.T) {
	root := cbRootClient(
		cbServiceObject(),
		cbAgentObject(servicesv1alpha1.PhasePublished),
		cbConfigObject(cbConfigV1, "v1", servicesv1alpha1.PhasePublished),
	)
	consumer := cbConsumerClient(cbEntitlementActive())
	r := newCapabilityReconciler(root, consumer)
	r.warnedAgents.Store(cbAgent, struct{}{})

	if _, err := r.Reconcile(context.Background(), cbRequest()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := getCapabilityBinding(t, consumer, cbAgent); !ok {
		t.Fatalf("expected the binding to project once the service resolves")
	}
	if _, warned := r.warnedAgents.Load(cbAgent); warned {
		t.Errorf("expected the warned record to be cleared")
	}
}
