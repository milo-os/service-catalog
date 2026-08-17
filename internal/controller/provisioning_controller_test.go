// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
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

var ipClassGVK = schema.GroupVersionKind{
	Group:   "networking.datumapis.com",
	Version: "v1alpha",
	Kind:    "IPClass",
}

func provScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = servicesv1alpha1.AddToScheme(s)
	s.AddKnownTypeWithName(ipClassGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(ipClassGVK.GroupVersion().WithKind("IPClassList"), &unstructured.UnstructuredList{})
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

func provConfig(kind servicesv1alpha1.GVKRef, selector metav1.LabelSelector) *servicesv1alpha1.ServiceConfiguration {
	return &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: provConfigName},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef: servicesv1alpha1.ServiceReference{Name: provServiceName},
			Phase:      servicesv1alpha1.PhasePublished,
			Provisioning: &servicesv1alpha1.ServiceProvisioningConfig{
				Resources: []servicesv1alpha1.ProvisionedResourceSpec{{
					Name: "public-classes",
					Projection: servicesv1alpha1.ResourceProjectionSpec{
						SourceProject: provSourceProject,
						Kind:          kind,
						Selector:      selector,
					},
				}},
			},
		},
		Status: servicesv1alpha1.ServiceConfigurationStatus{ServiceName: provServiceName},
	}
}

func ipClassKind() servicesv1alpha1.GVKRef {
	return servicesv1alpha1.GVKRef{Group: "networking.datumapis.com", Kind: "IPClass"}
}

func sharedSelector() metav1.LabelSelector {
	return metav1.LabelSelector{MatchLabels: map[string]string{"networking.datumapis.com/shared": "true"}}
}

func sourceClass(name string, labels map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(ipClassGVK)
	u.SetName(name)
	u.SetLabels(labels)
	_ = unstructured.SetNestedMap(u.Object, map[string]any{
		"prefixes": []any{"192.0.2.0/24"},
	}, "spec")
	return u
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

func listProjected(t *testing.T, c client.Client) *unstructured.UnstructuredList {
	t.Helper()
	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(ipClassGVK.GroupVersion().WithKind("IPClassList"))
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatalf("list projected classes: %v", err)
	}
	return &list
}

func getEntitlement(t *testing.T, c client.Client) *servicesv1alpha1.ServiceEntitlement {
	t.Helper()
	var ent servicesv1alpha1.ServiceEntitlement
	if err := c.Get(context.Background(), types.NamespacedName{Name: provEntitlement}, &ent); err != nil {
		t.Fatalf("get entitlement: %v", err)
	}
	return &ent
}

func TestProvisioningInstallsReferencesForActiveEntitlement(t *testing.T) {
	root := provClient(provConfig(ipClassKind(), sharedSelector()))
	source := provClient(
		sourceClass("public-unicast", map[string]string{"networking.datumapis.com/shared": "true"}),
		sourceClass("internal-only", nil),
	)
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{
		provConsumerProject: consumer,
		provSourceProject:   source,
	})
	provReconcile(t, r)

	list := listProjected(t, consumer)
	if len(list.Items) != 1 {
		t.Fatalf("expected exactly the selected class to be projected, got %d", len(list.Items))
	}

	got := list.Items[0]
	if want := "networking-datumapis-com-public-unicast"; got.GetName() != want {
		t.Errorf("projected name = %q, want %q (derived from service + source, not provider-chosen)", got.GetName(), want)
	}

	// The projected object must be a reference and must not carry the source's
	// addressing, or a consumer would hold a copy it could diverge from.
	source0, _, _ := unstructured.NestedMap(got.Object, "spec", "source")
	if source0["project"] != provSourceProject || source0["name"] != "public-unicast" {
		t.Errorf("projected spec.source = %+v, want a reference to the platform class", source0)
	}
	if _, found, _ := unstructured.NestedSlice(got.Object, "spec", "prefixes"); found {
		t.Error("projected object copied the source's prefixes; it must hold a reference only")
	}

	// Owner reference to the cluster-scoped entitlement is the teardown path
	// that does not depend on the project purger.
	if !ownedBy(got.GetOwnerReferences(), provEntitlementUID) {
		t.Errorf("projected object is not owned by the entitlement: %+v", got.GetOwnerReferences())
	}

	labels := got.GetLabels()
	if labels[labelManagedBy] != labelManagedByValue ||
		labels[labelEntitlementName] != provEntitlement ||
		labels[labelProvisionedResource] != "public-classes" {
		t.Errorf("projected object is not scoped for pruning: %+v", labels)
	}
}

// A project that never enabled the service, or whose request has not been
// approved, must receive nothing. Provisioning follows approval.
func TestProvisioningSkipsEntitlementNotActive(t *testing.T) {
	for _, phase := range []servicesv1alpha1.EntitlementPhase{
		servicesv1alpha1.EntitlementPhasePendingApproval,
		servicesv1alpha1.EntitlementPhaseRejected,
	} {
		root := provClient(provConfig(ipClassKind(), sharedSelector()))
		source := provClient(sourceClass("public-unicast", map[string]string{"networking.datumapis.com/shared": "true"}))
		consumer := provConsumerClient(provEntitlementObj(phase))

		r := newProvReconciler(root, map[string]client.Client{
			provConsumerProject: consumer,
			provSourceProject:   source,
		})
		provReconcile(t, r)

		if list := listProjected(t, consumer); len(list.Items) != 0 {
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
	root := provClient(provConfig(ipClassKind(), sharedSelector()))
	source := provClient(sourceClass("public-unicast", map[string]string{"networking.datumapis.com/shared": "true"}))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{
		provConsumerProject: consumer,
		provSourceProject:   source,
	})
	provReconcile(t, r)
	if len(listProjected(t, consumer).Items) != 1 {
		t.Fatalf("precondition failed: nothing was installed")
	}

	ent := getEntitlement(t, consumer)
	ent.Status.Phase = servicesv1alpha1.EntitlementPhaseRejected
	if err := consumer.Status().Update(context.Background(), ent); err != nil {
		t.Fatalf("update entitlement phase: %v", err)
	}

	provReconcile(t, r)
	if list := listProjected(t, consumer); len(list.Items) != 0 {
		t.Errorf("expected installed objects to be pruned, got %d", len(list.Items))
	}
}

// Story 2: a provider re-labelling its source objects reaches already-entitled
// projects without a new configuration.
func TestProvisioningConvergesOnSelectorChange(t *testing.T) {
	cfg := provConfig(ipClassKind(), sharedSelector())
	root := provClient(cfg)
	source := provClient(
		sourceClass("public-unicast", map[string]string{"networking.datumapis.com/shared": "true"}),
		sourceClass("anycast", map[string]string{"networking.datumapis.com/tier": "premium"}),
	)
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{
		provConsumerProject: consumer,
		provSourceProject:   source,
	})
	provReconcile(t, r)
	if got := listProjected(t, consumer).Items; len(got) != 1 || got[0].GetName() != "networking-datumapis-com-public-unicast" {
		t.Fatalf("precondition failed, projected: %+v", got)
	}

	var live servicesv1alpha1.ServiceConfiguration
	if err := root.Get(context.Background(), types.NamespacedName{Name: provConfigName}, &live); err != nil {
		t.Fatalf("get config: %v", err)
	}
	live.Spec.Provisioning.Resources[0].Projection.Selector = metav1.LabelSelector{
		MatchLabels: map[string]string{"networking.datumapis.com/tier": "premium"},
	}
	if err := root.Update(context.Background(), &live); err != nil {
		t.Fatalf("update config: %v", err)
	}

	provReconcile(t, r)
	got := listProjected(t, consumer).Items
	if len(got) != 1 || got[0].GetName() != "networking-datumapis-com-anycast" {
		t.Fatalf("selector change did not converge; projected: %+v", names(got))
	}
}

// The allowlist has to hold in the controller even when the declaration is
// already in etcd, because admission can be bypassed, removed, or predate a
// narrowing of the allowlist.
func TestProvisioningRefusesKindOutsideAllowlist(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind servicesv1alpha1.GVKRef
	}{
		{"unlisted", servicesv1alpha1.GVKRef{Group: "apps", Kind: "Deployment"}},
		{"categorically denied", servicesv1alpha1.GVKRef{Group: "iam.miloapis.com", Kind: "PolicyBinding"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := provClient(provConfig(tc.kind, sharedSelector()))
			source := provClient()
			consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

			r := newProvReconciler(root, map[string]client.Client{
				provConsumerProject: consumer,
				provSourceProject:   source,
			})
			provReconcile(t, r)

			ent := getEntitlement(t, consumer)
			if len(ent.Status.ProvisionedResources) != 1 {
				t.Fatalf("expected one ledger entry, got %+v", ent.Status.ProvisionedResources)
			}
			entry := ent.Status.ProvisionedResources[0]
			if entry.State != servicesv1alpha1.ProvisionedResourceStateUnprovisionable ||
				entry.Reason != reasonKindNotAllowed {
				t.Errorf("expected Unprovisionable/KindNotAllowed, got %+v", entry)
			}
			cond := apimeta.FindStatusCondition(ent.Status.Conditions, servicesv1alpha1.ConditionTypeProvisioned)
			if cond == nil || cond.Status != metav1.ConditionFalse {
				t.Errorf("expected Provisioned=False, got %+v", cond)
			}
		})
	}
}

// An empty selector converts to "match everything", which would project a
// provider's whole source project. It must be refused, not interpreted.
func TestProvisioningRefusesEmptySelector(t *testing.T) {
	root := provClient(provConfig(ipClassKind(), metav1.LabelSelector{}))
	source := provClient(sourceClass("public-unicast", nil))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{
		provConsumerProject: consumer,
		provSourceProject:   source,
	})
	provReconcile(t, r)

	if list := listProjected(t, consumer); len(list.Items) != 0 {
		t.Fatalf("empty selector projected %d objects", len(list.Items))
	}
	entry := getEntitlement(t, consumer).Status.ProvisionedResources[0]
	if entry.Reason != reasonSelectorEmpty {
		t.Errorf("expected SelectorEmpty, got %+v", entry)
	}
}

// The authorization gap must be legible on the object in a running system,
// not only in the source. A projection that the target API would authorize
// against its creator reports that the consumer does not hold the grant.
func TestProvisioningReportsUnestablishedAuthorization(t *testing.T) {
	root := provClient(provConfig(ipClassKind(), sharedSelector()))
	source := provClient(sourceClass("public-unicast", map[string]string{"networking.datumapis.com/shared": "true"}))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{
		provConsumerProject: consumer,
		provSourceProject:   source,
	})
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

// A source project that is not engaged is a retryable failure, and must not
// prune objects that are still legitimately owed.
func TestProvisioningDoesNotPruneWhenSourceUnreachable(t *testing.T) {
	root := provClient(provConfig(ipClassKind(), sharedSelector()))
	source := provClient(sourceClass("public-unicast", map[string]string{"networking.datumapis.com/shared": "true"}))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{
		provConsumerProject: consumer,
		provSourceProject:   source,
	})
	provReconcile(t, r)
	if len(listProjected(t, consumer).Items) != 1 {
		t.Fatal("precondition failed: nothing installed")
	}

	// Drop the source project from the engaged set.
	r = newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	if list := listProjected(t, consumer); len(list.Items) != 1 {
		t.Errorf("an unreachable source project pruned live objects; got %d", len(list.Items))
	}
	entry := getEntitlement(t, consumer).Status.ProvisionedResources[0]
	if entry.State != servicesv1alpha1.ProvisionedResourceStateFailed ||
		entry.Reason != reasonSourceProjectUnreachable {
		t.Errorf("expected Failed/SourceProjectUnreachable, got %+v", entry)
	}
}

// Withdrawing a declaration removes what it installed; that is the documented
// rollback path.
func TestProvisioningPrunesWhenDeclarationWithdrawn(t *testing.T) {
	root := provClient(provConfig(ipClassKind(), sharedSelector()))
	source := provClient(sourceClass("public-unicast", map[string]string{"networking.datumapis.com/shared": "true"}))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{
		provConsumerProject: consumer,
		provSourceProject:   source,
	})
	provReconcile(t, r)
	if len(listProjected(t, consumer).Items) != 1 {
		t.Fatal("precondition failed: nothing installed")
	}

	var live servicesv1alpha1.ServiceConfiguration
	if err := root.Get(context.Background(), types.NamespacedName{Name: provConfigName}, &live); err != nil {
		t.Fatalf("get config: %v", err)
	}
	live.Spec.Provisioning = nil
	if err := root.Update(context.Background(), &live); err != nil {
		t.Fatalf("update config: %v", err)
	}

	provReconcile(t, r)
	if list := listProjected(t, consumer); len(list.Items) != 0 {
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
	root := provClient(provConfig(ipClassKind(), sharedSelector()))
	source := provClient(sourceClass("public-unicast", map[string]string{"networking.datumapis.com/shared": "true"}))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{
		provConsumerProject: consumer,
		provSourceProject:   source,
	})
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

	root := provClient(provConfig(servicesv1alpha1.GVKRef{Group: "apps", Kind: "Deployment"}, sharedSelector()))
	consumer := provConsumerClient(ent)

	r := newProvReconciler(root, map[string]client.Client{
		provConsumerProject: consumer,
		provSourceProject:   provClient(),
	})
	provReconcile(t, r)

	ready := apimeta.FindStatusCondition(getEntitlement(t, consumer).Status.Conditions,
		servicesv1alpha1.ConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Errorf("a provisioning failure changed Ready: %+v", ready)
	}
}

// The active configuration is the most recently created Published one. The two
// existing fan-outs disagree on this rule; this one follows the location
// projection it generalizes.
func TestProvisioningSelectsLatestPublishedConfiguration(t *testing.T) {
	older := provConfig(ipClassKind(), sharedSelector())
	older.Name = "networking-v1"
	older.CreationTimestamp = metav1.NewTime(metav1.Now().Add(-time.Hour))

	newer := provConfig(ipClassKind(), metav1.LabelSelector{
		MatchLabels: map[string]string{"networking.datumapis.com/tier": "premium"},
	})
	newer.Name = "networking-v2"
	newer.CreationTimestamp = metav1.Now()

	root := provClient(older, newer)
	source := provClient(
		sourceClass("public-unicast", map[string]string{"networking.datumapis.com/shared": "true"}),
		sourceClass("anycast", map[string]string{"networking.datumapis.com/tier": "premium"}),
	)
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{
		provConsumerProject: consumer,
		provSourceProject:   source,
	})
	provReconcile(t, r)

	got := listProjected(t, consumer).Items
	if len(got) != 1 || got[0].GetName() != "networking-datumapis-com-anycast" {
		t.Errorf("expected the newest Published configuration to win, projected: %+v", names(got))
	}
}

func names(items []unstructured.Unstructured) []string {
	out := make([]string, 0, len(items))
	for i := range items {
		out = append(out, items[i].GetName())
	}
	return out
}
