// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// patchCapturingPortalClient wraps a client.Client and records every Patch
// call whose object is a ConsumerPortalPlugin or ProviderPortalPlugin
// (identified by GVK, since these are unstructured — see the doc comment on
// portalGroupVersion in user_interface_fanout.go for why). Delete/Get/List
// pass through to the wrapped client unchanged, so prune/cleanup behavior
// (which lists and deletes real objects) can still be exercised against a
// real fake-client store, same split as quota_fanout_test.go's
// patchCapturingClient.
type patchCapturingPortalClient struct {
	client.Client
	consumerPlugins []*unstructured.Unstructured
	providerPlugins []*unstructured.Unstructured
}

func (c *patchCapturingPortalClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return c.Client.Patch(ctx, obj, patch, opts...)
	}
	switch u.GetKind() {
	case "ConsumerPortalPlugin":
		c.consumerPlugins = append(c.consumerPlugins, u.DeepCopy())
	case "ProviderPortalPlugin":
		c.providerPlugins = append(c.providerPlugins, u.DeepCopy())
	}
	return nil
}

func newUserInterfaceFanOutScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = servicesv1alpha1.AddToScheme(s)
	return s
}

func newTestService(name, serviceName, displayName string) *servicesv1alpha1.Service {
	return &servicesv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: servicesv1alpha1.ServiceSpec{
			ServiceName: serviceName,
			DisplayName: displayName,
		},
	}
}

func basePluginAssets() servicesv1alpha1.PluginAssets {
	return servicesv1alpha1.PluginAssets{BaseURL: "https://plugin.example.com"}
}

func nestedStringOrEmpty(obj *unstructured.Unstructured, fields ...string) string {
	v, _, _ := unstructured.NestedString(obj.Object, fields...)
	return v
}

func nestedBool(obj *unstructured.Unstructured, fields ...string) bool {
	v, _, _ := unstructured.NestedBool(obj.Object, fields...)
	return v
}

// TestUserInterfaceFanOut_ConsumerOnly verifies that a Consumer-only spec
// applies a ConsumerPortalPlugin and no ProviderPortalPlugin.
func TestUserInterfaceFanOut_ConsumerOnly(t *testing.T) {
	svc := newTestService("compute", "compute.datumapis.com", "Compute")
	sc := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "compute-config", UID: "uid-consumer-only"},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef: servicesv1alpha1.ServiceReference{Name: "compute"},
			Phase:      servicesv1alpha1.PhasePublished,
			UserInterface: &servicesv1alpha1.UserInterfaceSpec{
				Consumer: &servicesv1alpha1.ConsumerUserInterfaceSpec{
					Assets:     basePluginAssets(),
					Visibility: servicesv1alpha1.PluginVisibility{Entitlement: "None"},
				},
			},
		},
	}

	scheme := newUserInterfaceFanOutScheme()
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc).Build()
	capturing := &patchCapturingPortalClient{Client: base}
	fanOut := &UserInterfaceFanOut{Client: capturing, Scheme: scheme}

	if err := fanOut.Reconcile(context.Background(), sc); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(capturing.consumerPlugins) != 1 {
		t.Fatalf("expected 1 ConsumerPortalPlugin patch, got %d", len(capturing.consumerPlugins))
	}
	if len(capturing.providerPlugins) != 0 {
		t.Fatalf("expected 0 ProviderPortalPlugin patches, got %d", len(capturing.providerPlugins))
	}

	cp := capturing.consumerPlugins[0]
	if got := nestedStringOrEmpty(cp, "spec", "slug"); got != "compute-datumapis-com" {
		t.Errorf("spec.slug = %q, want %q", got, "compute-datumapis-com")
	}
	if got := nestedStringOrEmpty(cp, "spec", "displayName"); got != "Compute" {
		t.Errorf("spec.displayName = %q, want %q", got, "Compute")
	}
	if got := nestedStringOrEmpty(cp, "spec", "assets", "baseURL"); got != "https://plugin.example.com" {
		t.Errorf("spec.assets.baseURL = %q, want %q", got, "https://plugin.example.com")
	}
	if got := cp.GetLabels()[labelOwnerService]; got != "compute.datumapis.com" {
		t.Errorf("label %q = %q, want %q", labelOwnerService, got, "compute.datumapis.com")
	}
	if got := cp.GetLabels()[labelManagedBy]; got != labelManagedByValue {
		t.Errorf("label %q = %q, want %q", labelManagedBy, got, labelManagedByValue)
	}
	if len(cp.GetOwnerReferences()) == 0 {
		t.Error("OwnerReferences is empty; ConsumerPortalPlugin won't be garbage-collected with the ServiceConfiguration")
	}
}

// TestUserInterfaceFanOut_ProviderOnly mirrors ConsumerOnly for the
// staff-portal side, and additionally checks Suspend passthrough.
func TestUserInterfaceFanOut_ProviderOnly(t *testing.T) {
	svc := newTestService("compute", "compute.datumapis.com", "Compute")
	sc := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "compute-config", UID: "uid-provider-only"},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef: servicesv1alpha1.ServiceReference{Name: "compute"},
			Phase:      servicesv1alpha1.PhasePublished,
			UserInterface: &servicesv1alpha1.UserInterfaceSpec{
				Provider: &servicesv1alpha1.ProviderUserInterfaceSpec{
					Suspend: true,
					Assets:  basePluginAssets(),
				},
			},
		},
	}

	scheme := newUserInterfaceFanOutScheme()
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc).Build()
	capturing := &patchCapturingPortalClient{Client: base}
	fanOut := &UserInterfaceFanOut{Client: capturing, Scheme: scheme}

	if err := fanOut.Reconcile(context.Background(), sc); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(capturing.providerPlugins) != 1 {
		t.Fatalf("expected 1 ProviderPortalPlugin patch, got %d", len(capturing.providerPlugins))
	}
	if len(capturing.consumerPlugins) != 0 {
		t.Fatalf("expected 0 ConsumerPortalPlugin patches, got %d", len(capturing.consumerPlugins))
	}

	pp := capturing.providerPlugins[0]
	if !nestedBool(pp, "spec", "suspend") {
		t.Error("spec.suspend = false, want true (should pass through from ProviderUserInterfaceSpec.Suspend)")
	}
	if got := nestedStringOrEmpty(pp, "spec", "slug"); got != "compute-datumapis-com" {
		t.Errorf("spec.slug = %q, want %q", got, "compute-datumapis-com")
	}
}

// TestUserInterfaceFanOut_Both verifies that setting both Consumer and
// Provider applies both plugin Kinds from a single ServiceConfiguration.
func TestUserInterfaceFanOut_Both(t *testing.T) {
	svc := newTestService("compute", "compute.datumapis.com", "Compute")
	sc := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "compute-config", UID: "uid-both"},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef: servicesv1alpha1.ServiceReference{Name: "compute"},
			Phase:      servicesv1alpha1.PhasePublished,
			UserInterface: &servicesv1alpha1.UserInterfaceSpec{
				Consumer: &servicesv1alpha1.ConsumerUserInterfaceSpec{
					Assets:     basePluginAssets(),
					Visibility: servicesv1alpha1.PluginVisibility{Entitlement: "Required"},
				},
				Provider: &servicesv1alpha1.ProviderUserInterfaceSpec{
					Assets: basePluginAssets(),
				},
			},
		},
	}

	scheme := newUserInterfaceFanOutScheme()
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc).Build()
	capturing := &patchCapturingPortalClient{Client: base}
	fanOut := &UserInterfaceFanOut{Client: capturing, Scheme: scheme}

	if err := fanOut.Reconcile(context.Background(), sc); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(capturing.consumerPlugins) != 1 {
		t.Fatalf("expected 1 ConsumerPortalPlugin patch, got %d", len(capturing.consumerPlugins))
	}
	if len(capturing.providerPlugins) != 1 {
		t.Fatalf("expected 1 ProviderPortalPlugin patch, got %d", len(capturing.providerPlugins))
	}
	if got := nestedStringOrEmpty(capturing.consumerPlugins[0], "spec", "visibility", "entitlement"); got != "Required" {
		t.Errorf("spec.visibility.entitlement = %q, want %q", got, "Required")
	}
}

// TestUserInterfaceFanOut_NilUserInterface verifies Reconcile is a full
// no-op when spec.userInterface is nil.
func TestUserInterfaceFanOut_NilUserInterface(t *testing.T) {
	svc := newTestService("compute", "compute.datumapis.com", "Compute")
	sc := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "compute-config", UID: "uid-nil"},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef: servicesv1alpha1.ServiceReference{Name: "compute"},
			Phase:      servicesv1alpha1.PhasePublished,
		},
	}

	scheme := newUserInterfaceFanOutScheme()
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc).Build()
	capturing := &patchCapturingPortalClient{Client: base}
	fanOut := &UserInterfaceFanOut{Client: capturing, Scheme: scheme}

	if err := fanOut.Reconcile(context.Background(), sc); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(capturing.consumerPlugins) != 0 || len(capturing.providerPlugins) != 0 {
		t.Fatalf("expected no patches for a nil UserInterface, got consumer=%d provider=%d",
			len(capturing.consumerPlugins), len(capturing.providerPlugins))
	}
}

// TestUserInterfaceFanOut_DraftSkipped verifies Reconcile is a no-op while
// the ServiceConfiguration is still a Draft, even with UserInterface set.
func TestUserInterfaceFanOut_DraftSkipped(t *testing.T) {
	svc := newTestService("compute", "compute.datumapis.com", "Compute")
	sc := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "compute-config", UID: "uid-draft"},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef: servicesv1alpha1.ServiceReference{Name: "compute"},
			Phase:      servicesv1alpha1.PhaseDraft,
			UserInterface: &servicesv1alpha1.UserInterfaceSpec{
				Consumer: &servicesv1alpha1.ConsumerUserInterfaceSpec{
					Assets:     basePluginAssets(),
					Visibility: servicesv1alpha1.PluginVisibility{Entitlement: "None"},
				},
			},
		},
	}

	scheme := newUserInterfaceFanOutScheme()
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc).Build()
	capturing := &patchCapturingPortalClient{Client: base}
	fanOut := &UserInterfaceFanOut{Client: capturing, Scheme: scheme}

	if err := fanOut.Reconcile(context.Background(), sc); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(capturing.consumerPlugins) != 0 {
		t.Fatalf("expected no patches while Draft, got %d", len(capturing.consumerPlugins))
	}
}

func newConsumerPortalPlugin(name string, labels map[string]string, owner metav1.OwnerReference, slug, displayName, baseURL string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(portalGVK("ConsumerPortalPlugin"))
	u.SetName(name)
	u.SetLabels(labels)
	u.SetOwnerReferences([]metav1.OwnerReference{owner})
	_ = unstructured.SetNestedMap(u.Object, map[string]interface{}{
		"slug":        slug,
		"displayName": displayName,
		"assets":      map[string]interface{}{"baseURL": baseURL},
		"visibility":  map[string]interface{}{"entitlement": "None"},
	}, "spec")
	return u
}

// TestUserInterfaceFanOut_PruneOnRemoval verifies that removing
// spec.userInterface.consumer deletes a previously-applied ConsumerPortalPlugin
// owned by this ServiceConfiguration, without touching one owned by another.
func TestUserInterfaceFanOut_PruneOnRemoval(t *testing.T) {
	svc := newTestService("compute", "compute.datumapis.com", "Compute")
	sc := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "compute-config", UID: "uid-prune"},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef: servicesv1alpha1.ServiceReference{Name: "compute"},
			Phase:      servicesv1alpha1.PhasePublished,
			// UserInterface is nil: the previously-applied plugin below should
			// be pruned since it's no longer desired.
		},
	}

	staleOwned := newConsumerPortalPlugin(
		"compute-datumapis-com",
		map[string]string{labelManagedBy: labelManagedByValue, labelOwnerService: "compute.datumapis.com"},
		metav1.OwnerReference{UID: sc.UID, Name: sc.Name, Kind: "ServiceConfiguration", APIVersion: "services.miloapis.com/v1alpha1"},
		"compute-datumapis-com", "Compute", "https://plugin.example.com",
	)
	ownedByOther := newConsumerPortalPlugin(
		"other-service",
		map[string]string{labelManagedBy: labelManagedByValue, labelOwnerService: "other.datumapis.com"},
		metav1.OwnerReference{UID: types.UID("uid-other-sc"), Name: "other-config", Kind: "ServiceConfiguration", APIVersion: "services.miloapis.com/v1alpha1"},
		"other-service", "Other", "https://other.example.com",
	)

	scheme := newUserInterfaceFanOutScheme()
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc, staleOwned, ownedByOther).Build()
	fanOut := &UserInterfaceFanOut{Client: base, Scheme: scheme}

	if err := fanOut.Reconcile(context.Background(), sc); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(portalGVK("ConsumerPortalPlugin"))
	err := base.Get(context.Background(), client.ObjectKey{Name: "compute-datumapis-com"}, got)
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected stale ConsumerPortalPlugin owned by this ServiceConfiguration to be pruned, got err=%v", err)
	}

	got2 := &unstructured.Unstructured{}
	got2.SetGroupVersionKind(portalGVK("ConsumerPortalPlugin"))
	if err := base.Get(context.Background(), client.ObjectKey{Name: "other-service"}, got2); err != nil {
		t.Errorf("ConsumerPortalPlugin owned by a different ServiceConfiguration should not be pruned: %v", err)
	}
}

// TestUserInterfaceFanOut_Cleanup verifies Cleanup deletes both
// ConsumerPortalPlugin and ProviderPortalPlugin owned by the
// ServiceConfiguration, regardless of what spec.userInterface currently says.
func TestUserInterfaceFanOut_Cleanup(t *testing.T) {
	sc := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "compute-config", UID: "uid-cleanup"},
	}

	ownerRef := metav1.OwnerReference{UID: sc.UID, Name: sc.Name, Kind: "ServiceConfiguration", APIVersion: "services.miloapis.com/v1alpha1"}
	labels := map[string]string{labelManagedBy: labelManagedByValue, labelOwnerService: "compute.datumapis.com"}
	consumer := newConsumerPortalPlugin("compute-datumapis-com", labels, ownerRef, "compute-datumapis-com", "Compute", "https://plugin.example.com")

	provider := &unstructured.Unstructured{}
	provider.SetGroupVersionKind(portalGVK("ProviderPortalPlugin"))
	provider.SetName("compute-datumapis-com")
	provider.SetLabels(labels)
	provider.SetOwnerReferences([]metav1.OwnerReference{ownerRef})
	_ = unstructured.SetNestedMap(provider.Object, map[string]interface{}{
		"slug": "compute-datumapis-com", "displayName": "Compute",
		"assets": map[string]interface{}{"baseURL": "https://plugin.example.com"},
	}, "spec")

	scheme := newUserInterfaceFanOutScheme()
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(consumer, provider).Build()
	fanOut := &UserInterfaceFanOut{Client: base, Scheme: scheme}

	if err := fanOut.Cleanup(context.Background(), sc); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	gotConsumer := &unstructured.Unstructured{}
	gotConsumer.SetGroupVersionKind(portalGVK("ConsumerPortalPlugin"))
	if err := base.Get(context.Background(), client.ObjectKey{Name: "compute-datumapis-com"}, gotConsumer); !apierrors.IsNotFound(err) {
		t.Errorf("expected ConsumerPortalPlugin to be deleted by Cleanup, got err=%v", err)
	}
	gotProvider := &unstructured.Unstructured{}
	gotProvider.SetGroupVersionKind(portalGVK("ProviderPortalPlugin"))
	if err := base.Get(context.Background(), client.ObjectKey{Name: "compute-datumapis-com"}, gotProvider); !apierrors.IsNotFound(err) {
		t.Errorf("expected ProviderPortalPlugin to be deleted by Cleanup, got err=%v", err)
	}
}
