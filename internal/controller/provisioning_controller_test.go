// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const (
	provConsumerProject = "consumer-proj"
	provSourceProject   = "networking-platform"
	provEntitlement     = "networking"
	provServiceName     = "networking.datumapis.com"
	provConfigName      = "networking-datumapis-com"
	provEntitlementUID  = types.UID("ent-networking-uid")
)

var (
	ipClassGVK = schema.GroupVersionKind{
		Group:   "ipam.miloapis.com",
		Version: "v1alpha1",
		Kind:    "IPClass",
	}
	ipPoolGVK = ipClassGVK.GroupVersion().WithKind("IPPool")
)

func provScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = servicesv1alpha1.AddToScheme(s)
	for _, gvk := range []schema.GroupVersionKind{ipClassGVK, ipPoolGVK} {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	return s
}

func provClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(provScheme()).
		WithObjects(objs...).
		Build()
}

// provConsumerClient keeps ServiceEntitlement's status subresource enabled so
// the ledger patch exercises the same path it does against a real API server.
func provConsumerClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(provScheme()).
		WithObjects(objs...).
		WithStatusSubresource(&servicesv1alpha1.ServiceEntitlement{}).
		Build()
}

func provEntitlementObj(phase servicesv1alpha1.EntitlementPhase) *servicesv1alpha1.ServiceEntitlement {
	return &servicesv1alpha1.ServiceEntitlement{
		ObjectMeta: metav1.ObjectMeta{Name: provEntitlement, UID: provEntitlementUID},
		Spec: servicesv1alpha1.ServiceEntitlementSpec{
			ServiceRef: servicesv1alpha1.ServiceRef{Name: provServiceName},
		},
		Status: servicesv1alpha1.ServiceEntitlementStatus{Phase: phase},
	}
}

func provObject(body string) servicesv1alpha1.ProvisionedObject {
	return servicesv1alpha1.ProvisionedObject{RawExtension: runtime.RawExtension{Raw: []byte(body)}}
}

// The shape IPAM gives a cross-project class reference: spec.source, keyed by
// project and name. The provider writes it; the platform does not know it.
func ipClassRef(name string) servicesv1alpha1.ProvisionedObject {
	return provObject(fmt.Sprintf(`{
		"apiVersion": "ipam.miloapis.com/v1alpha1",
		"kind": "IPClass",
		"metadata": {"name": %q},
		"spec": {"source": {"project": %q, "name": %q}}
	}`, name, provSourceProject, name))
}

func provConfig(objects ...servicesv1alpha1.ProvisionedObject) *servicesv1alpha1.ServiceConfiguration {
	return &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: provConfigName},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef: servicesv1alpha1.ServiceReference{Name: provServiceName},
			Phase:      servicesv1alpha1.PhasePublished,
			Provisioning: &servicesv1alpha1.ServiceProvisioningConfig{
				Resources: []servicesv1alpha1.ProvisionedResourceSpec{{
					Name:    "address-classes",
					Objects: objects,
				}},
			},
		},
		Status: servicesv1alpha1.ServiceConfigurationStatus{ServiceName: provServiceName},
	}
}

func newProvReconciler(root client.Client, clusters map[string]client.Client) *ProvisioningReconciler {
	mgr := newTestManager()
	for name, c := range clusters {
		mgr.add(name, c)
	}
	return &ProvisioningReconciler{rootClient: root, Manager: mgr, Scheme: provScheme()}
}

func provReconcile(t *testing.T, r *ProvisioningReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), mcreconcile.Request{
		Request:     ctrl.Request{NamespacedName: types.NamespacedName{Name: provEntitlement}},
		ClusterName: provConsumerProject,
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func listInstalled(t *testing.T, c client.Client, gvk schema.GroupVersionKind) *unstructured.UnstructuredList {
	t.Helper()
	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatalf("list installed %s: %v", gvk.Kind, err)
	}
	return &list
}

func listClasses(t *testing.T, c client.Client) *unstructured.UnstructuredList {
	t.Helper()
	return listInstalled(t, c, ipClassGVK)
}

func getEntitlement(t *testing.T, c client.Client) *servicesv1alpha1.ServiceEntitlement {
	t.Helper()
	var ent servicesv1alpha1.ServiceEntitlement
	if err := c.Get(context.Background(), types.NamespacedName{Name: provEntitlement}, &ent); err != nil {
		t.Fatalf("get entitlement: %v", err)
	}
	return &ent
}

func setConfig(t *testing.T, root client.Client, mutate func(*servicesv1alpha1.ServiceConfiguration)) {
	t.Helper()
	var live servicesv1alpha1.ServiceConfiguration
	if err := root.Get(context.Background(), types.NamespacedName{Name: provConfigName}, &live); err != nil {
		t.Fatalf("get config: %v", err)
	}
	mutate(&live)
	if err := root.Update(context.Background(), &live); err != nil {
		t.Fatalf("update config: %v", err)
	}
}

func TestProvisioningInstallsDeclaredObjectsForActiveEntitlement(t *testing.T) {
	root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6")))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	list := listClasses(t, consumer)
	if len(list.Items) != 1 {
		t.Fatalf("expected the declared class to be installed, got %d", len(list.Items))
	}

	got := list.Items[0]
	// The provider names the object; the platform installs it verbatim.
	if got.GetName() != "tenant-endpoint-ipv6" {
		t.Errorf("installed name = %q, want the name the declaration carries", got.GetName())
	}
	source, _, _ := unstructured.NestedMap(got.Object, "spec", "source")
	if source["project"] != provSourceProject || source["name"] != "tenant-endpoint-ipv6" {
		t.Errorf("installed spec.source = %+v, want the reference the provider wrote", source)
	}

	// Owner reference to the cluster-scoped entitlement is the teardown path
	// that does not depend on the project purger.
	if !ownedBy(got.GetOwnerReferences(), provEntitlementUID) {
		t.Errorf("installed object is not owned by the entitlement: %+v", got.GetOwnerReferences())
	}

	// Provenance moved entirely into the labels once the name stopped carrying
	// it, so this is what answers "which service put this here".
	labels := got.GetLabels()
	if labels[labelManagedBy] != labelManagedByValue ||
		labels[labelServiceName] != encodeName(provServiceName) ||
		labels[labelEntitlementName] != provEntitlement ||
		labels[labelProvisionedResource] != "address-classes" {
		t.Errorf("installed object does not record its provenance: %+v", labels)
	}

	entry := getEntitlement(t, consumer).Status.ProvisionedResources[0]
	if entry.State != servicesv1alpha1.ProvisionedResourceStateInstalled || entry.ObjectCount != 1 {
		t.Fatalf("ledger does not report the install: %+v", entry)
	}
	if len(entry.Kinds) != 1 || entry.Kinds[0].Kind != "IPClass" || entry.Kinds[0].Version != "v1alpha1" {
		t.Errorf("ledger does not record what was installed: %+v", entry.Kinds)
	}
}

// A project that never enabled the service, or whose request has not been
// approved, must receive nothing. Provisioning follows approval.
func TestProvisioningSkipsEntitlementNotActive(t *testing.T) {
	for _, phase := range []servicesv1alpha1.EntitlementPhase{
		servicesv1alpha1.EntitlementPhasePendingApproval,
		servicesv1alpha1.EntitlementPhaseRejected,
	} {
		root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6")))
		consumer := provConsumerClient(provEntitlementObj(phase))

		r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
		provReconcile(t, r)

		if list := listClasses(t, consumer); len(list.Items) != 0 {
			t.Errorf("phase %s: expected nothing installed, got %d objects", phase, len(list.Items))
		}
		cond := apimeta.FindStatusCondition(getEntitlement(t, consumer).Status.Conditions,
			servicesv1alpha1.ConditionTypeProvisioned)
		if cond == nil || cond.Status != metav1.ConditionFalse ||
			cond.Reason != servicesv1alpha1.ReasonEntitlementNotActive {
			t.Errorf("phase %s: expected Provisioned=False/EntitlementNotActive, got %+v", phase, cond)
		}
	}
}

// Losing Active must remove what was installed, not merely stop adding to it.
func TestProvisioningPrunesWhenEntitlementLeavesActive(t *testing.T) {
	root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6")))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)
	if len(listClasses(t, consumer).Items) != 1 {
		t.Fatalf("precondition failed: nothing was installed")
	}

	ent := getEntitlement(t, consumer)
	ent.Status.Phase = servicesv1alpha1.EntitlementPhaseRejected
	if err := consumer.Status().Update(context.Background(), ent); err != nil {
		t.Fatalf("update entitlement phase: %v", err)
	}

	provReconcile(t, r)
	if list := listClasses(t, consumer); len(list.Items) != 0 {
		t.Errorf("expected installed objects to be pruned, got %d", len(list.Items))
	}
}

// Editing the declaration is how a provider changes what consumers hold, so the
// object it stops declaring has to go — including when the change is to the
// object's kind, which no longer appears in the declaration once removed.
func TestProvisioningConvergesOnDeclarationChange(t *testing.T) {
	root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6")))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)
	if got := listClasses(t, consumer).Items; len(got) != 1 {
		t.Fatalf("precondition failed, installed: %+v", names(got))
	}

	setConfig(t, root, func(sc *servicesv1alpha1.ServiceConfiguration) {
		sc.Spec.Provisioning.Resources[0].Objects = []servicesv1alpha1.ProvisionedObject{
			provObject(`{"apiVersion":"ipam.miloapis.com/v1alpha1","kind":"IPPool","metadata":{"name":"tenant-pool"}}`),
		}
	})
	provReconcile(t, r)

	if got := listClasses(t, consumer).Items; len(got) != 0 {
		t.Errorf("the withdrawn class survived a kind change: %+v", names(got))
	}
	if got := listInstalled(t, consumer, ipPoolGVK).Items; len(got) != 1 || got[0].GetName() != "tenant-pool" {
		t.Errorf("the new object did not arrive: %+v", names(got))
	}
}

// The declaration is applied, so a consumer edit to a field it states is
// reverted rather than left standing.
func TestProvisioningRestoresConsumerEdits(t *testing.T) {
	root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6")))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	edited := listClasses(t, consumer).Items[0]
	if err := unstructured.SetNestedMap(edited.Object, map[string]any{
		"project": "somewhere-else", "name": "somewhere-else",
	}, "spec", "source"); err != nil {
		t.Fatalf("edit installed object: %v", err)
	}
	if err := consumer.Update(context.Background(), &edited); err != nil {
		t.Fatalf("update installed object: %v", err)
	}

	provReconcile(t, r)
	source, _, _ := unstructured.NestedMap(listClasses(t, consumer).Items[0].Object, "spec", "source")
	if source["project"] != provSourceProject {
		t.Errorf("a consumer edit survived the next reconcile: %+v", source)
	}
}

// The shape of an embedded object has to hold in the controller even when the
// declaration is already in etcd, because admission can be bypassed, removed,
// or predate the schema that would have refused it.
func TestProvisioningRefusesObjectsItWillNotWrite(t *testing.T) {
	for _, tc := range []struct {
		name string
		obj  servicesv1alpha1.ProvisionedObject
	}{
		{"core group", provObject(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"x"}}`)},
		{"no version", provObject(`{"apiVersion":"ipam.miloapis.com","kind":"IPClass","metadata":{"name":"x"}}`)},
		{"no name", provObject(`{"apiVersion":"ipam.miloapis.com/v1alpha1","kind":"IPClass","metadata":{}}`)},
		{"namespaced", provObject(`{"apiVersion":"ipam.miloapis.com/v1alpha1","kind":"IPClass","metadata":{"name":"x","namespace":"kube-system"}}`)},
		{"owner reference", provObject(`{"apiVersion":"ipam.miloapis.com/v1alpha1","kind":"IPClass","metadata":{"name":"x","ownerReferences":[]}}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := provClient(provConfig(tc.obj))
			consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

			r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
			provReconcile(t, r)

			if list := listClasses(t, consumer); len(list.Items) != 0 {
				t.Fatalf("a refused declaration installed %d objects", len(list.Items))
			}
			ent := getEntitlement(t, consumer)
			if len(ent.Status.ProvisionedResources) != 1 {
				t.Fatalf("expected one ledger entry, got %+v", ent.Status.ProvisionedResources)
			}
			entry := ent.Status.ProvisionedResources[0]
			if entry.State != servicesv1alpha1.ProvisionedResourceStateUnprovisionable ||
				entry.Reason != reasonObjectInvalid {
				t.Errorf("expected Unprovisionable/ObjectInvalid, got %+v", entry)
			}
			cond := apimeta.FindStatusCondition(ent.Status.Conditions, servicesv1alpha1.ConditionTypeProvisioned)
			if cond == nil || cond.Status != metav1.ConditionFalse {
				t.Errorf("expected Provisioned=False, got %+v", cond)
			}
		})
	}
}

// A declaration that stops resolving must not erase the record teardown depends
// on, or its objects are stranded in every project that holds them.
func TestProvisioningKeepsTheLedgerWhenADeclarationStopsResolving(t *testing.T) {
	root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6")))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	setConfig(t, root, func(sc *servicesv1alpha1.ServiceConfiguration) {
		sc.Spec.Provisioning.Resources[0].Objects = []servicesv1alpha1.ProvisionedObject{
			provObject(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"x"}}`),
		}
	})
	provReconcile(t, r)

	entry := getEntitlement(t, consumer).Status.ProvisionedResources[0]
	if len(entry.Kinds) != 1 || entry.Kinds[0].Kind != "IPClass" {
		t.Fatalf("the ledger forgot what is still installed: %+v", entry.Kinds)
	}

	// And withdrawing the declaration still finds it.
	setConfig(t, root, func(sc *servicesv1alpha1.ServiceConfiguration) { sc.Spec.Provisioning = nil })
	provReconcile(t, r)
	if list := listClasses(t, consumer); len(list.Items) != 0 {
		t.Errorf("teardown left %d objects behind", len(list.Items))
	}
}

// One declaration installing more than the cap is refused whole, never
// truncated: a truncated fan-out looks like a working system.
func TestProvisioningRefusesMoreObjectsThanTheCap(t *testing.T) {
	objects := make([]servicesv1alpha1.ProvisionedObject, 0, maxProvisionedObjectsPerResource+1)
	for i := 0; i <= maxProvisionedObjectsPerResource; i++ {
		objects = append(objects, ipClassRef(fmt.Sprintf("class-%d", i)))
	}
	root := provClient(provConfig(objects...))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	if list := listClasses(t, consumer); len(list.Items) != 0 {
		t.Fatalf("an oversized declaration installed %d objects", len(list.Items))
	}
	if entry := getEntitlement(t, consumer).Status.ProvisionedResources[0]; entry.Reason != reasonTooManyObjects {
		t.Errorf("expected TooManyObjects, got %+v", entry)
	}
}

// The authorization gap must be legible on the object in a running system, not
// only in the source. An object the owning API would authorize against its
// creator reports that the consumer does not hold the grant.
func TestProvisioningReportsUnestablishedAuthorization(t *testing.T) {
	root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6")))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	entry := getEntitlement(t, consumer).Status.ProvisionedResources[0]
	if entry.State != servicesv1alpha1.ProvisionedResourceStateInstalled {
		t.Fatalf("expected the resource to install, got %+v", entry)
	}
	if entry.AuthorizationEstablished {
		t.Error("IPAM authorizes the reference against its creator and nothing here establishes " +
			"the consumer's own grant; authorizationEstablished must be false")
	}
	if !strings.Contains(entry.Message, "platform's authority") {
		t.Errorf("ledger message does not explain whose authority the object rests on: %q", entry.Message)
	}
}

// Withdrawing a declaration removes what it installed; that is the documented
// rollback path.
func TestProvisioningPrunesWhenDeclarationWithdrawn(t *testing.T) {
	root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6")))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)
	if len(listClasses(t, consumer).Items) != 1 {
		t.Fatal("precondition failed: nothing installed")
	}

	setConfig(t, root, func(sc *servicesv1alpha1.ServiceConfiguration) { sc.Spec.Provisioning = nil })
	provReconcile(t, r)

	if list := listClasses(t, consumer); len(list.Items) != 0 {
		t.Errorf("withdrawing the declaration left %d objects behind", len(list.Items))
	}
	cond := apimeta.FindStatusCondition(getEntitlement(t, consumer).Status.Conditions,
		servicesv1alpha1.ConditionTypeProvisioned)
	if cond == nil || cond.Reason != servicesv1alpha1.ReasonNothingToProvision {
		t.Errorf("expected NothingToProvision, got %+v", cond)
	}
}

// The ledger records when provisioning last ran, so a fan-out that has silently
// stopped is visible on the object rather than inferable from someone else's
// incident.
func TestProvisioningRecordsLastEvaluation(t *testing.T) {
	root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6")))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	if getEntitlement(t, consumer).Status.LastProvisioningEvaluation == nil {
		t.Error("lastProvisioningEvaluation was not recorded")
	}
}

// Delivery and access are different facts. A provisioning failure must not
// touch Ready, or it would read as a denial.
func TestProvisioningLeavesReadyConditionAlone(t *testing.T) {
	ent := provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive)
	ent.Status.Conditions = []metav1.Condition{{
		Type:               servicesv1alpha1.ConditionTypeReady,
		Status:             metav1.ConditionTrue,
		Reason:             servicesv1alpha1.ReasonEntitlementActive,
		LastTransitionTime: metav1.Now(),
	}}

	root := provClient(provConfig(provObject(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"x"}}`)))
	consumer := provConsumerClient(ent)

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	ready := apimeta.FindStatusCondition(getEntitlement(t, consumer).Status.Conditions,
		servicesv1alpha1.ConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Errorf("a provisioning failure changed Ready: %+v", ready)
	}
}

// The active configuration is the most recently created Published one. The two
// existing fan-outs disagree on this rule; this one follows the location
// projection it sits beside.
func TestProvisioningSelectsLatestPublishedConfiguration(t *testing.T) {
	older := provConfig(ipClassRef("tenant-endpoint-ipv6"))
	older.Name = "networking-v1"
	older.CreationTimestamp = metav1.NewTime(metav1.Now().Add(-time.Hour))

	newer := provConfig(ipClassRef("tenant-endpoint-ipv4"))
	newer.Name = "networking-v2"
	newer.CreationTimestamp = metav1.Now()

	root := provClient(older, newer)
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	got := listClasses(t, consumer).Items
	if len(got) != 1 || got[0].GetName() != "tenant-endpoint-ipv4" {
		t.Errorf("expected the newest Published configuration to win, installed: %+v", names(got))
	}
}

func names(items []unstructured.Unstructured) []string {
	out := make([]string, 0, len(items))
	for i := range items {
		out = append(out, items[i].GetName())
	}
	return out
}
