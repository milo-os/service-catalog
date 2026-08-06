// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	portalv1alpha1 "go.miloapis.com/milo/pkg/apis/portal/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// patchCapturingPortalClient wraps a client.Client and records every Patch
// call whose object is a *portalv1alpha1.ConsumerPortalPlugin or
// *portalv1alpha1.ProviderPortalPlugin. Delete/Get/List pass through to the
// wrapped client unchanged, so prune/cleanup behavior (which lists and
// deletes real objects) can still be exercised against a real fake-client
// store, same split as quota_fanout_test.go's patchCapturingClient.
type patchCapturingPortalClient struct {
	client.Client
	consumerPlugins []*portalv1alpha1.ConsumerPortalPlugin
	providerPlugins []*portalv1alpha1.ProviderPortalPlugin
}

func (c *patchCapturingPortalClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	switch o := obj.(type) {
	case *portalv1alpha1.ConsumerPortalPlugin:
		c.consumerPlugins = append(c.consumerPlugins, o.DeepCopy())
	case *portalv1alpha1.ProviderPortalPlugin:
		c.providerPlugins = append(c.providerPlugins, o.DeepCopy())
	}
	return nil
}

func newUserInterfaceFanOutScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = servicesv1alpha1.AddToScheme(s)
	_ = portalv1alpha1.AddToScheme(s)
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
	if cp.Spec.Slug != "compute-datumapis-com" {
		t.Errorf("Slug = %q, want %q", cp.Spec.Slug, "compute-datumapis-com")
	}
	if cp.Spec.DisplayName != "Compute" {
		t.Errorf("DisplayName = %q, want %q", cp.Spec.DisplayName, "Compute")
	}
	if cp.Spec.Assets.BaseURL != "https://plugin.example.com" {
		t.Errorf("Assets.BaseURL = %q, want %q", cp.Spec.Assets.BaseURL, "https://plugin.example.com")
	}
	if cp.Labels[labelOwnerService] != "compute.datumapis.com" {
		t.Errorf("label %q = %q, want %q", labelOwnerService, cp.Labels[labelOwnerService], "compute.datumapis.com")
	}
	if cp.Labels[labelManagedBy] != labelManagedByValue {
		t.Errorf("label %q = %q, want %q", labelManagedBy, cp.Labels[labelManagedBy], labelManagedByValue)
	}
	if len(cp.OwnerReferences) == 0 {
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
	if !pp.Spec.Suspend {
		t.Error("Spec.Suspend = false, want true (should pass through from ProviderUserInterfaceSpec.Suspend)")
	}
	if pp.Spec.Slug != "compute-datumapis-com" {
		t.Errorf("Slug = %q, want %q", pp.Spec.Slug, "compute-datumapis-com")
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
	if capturing.consumerPlugins[0].Spec.Visibility.Entitlement != portalv1alpha1.PluginEntitlementRequired {
		t.Errorf("Visibility.Entitlement = %q, want %q",
			capturing.consumerPlugins[0].Spec.Visibility.Entitlement, portalv1alpha1.PluginEntitlementRequired)
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

	staleOwned := &portalv1alpha1.ConsumerPortalPlugin{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "compute-datumapis-com",
			Labels: map[string]string{labelManagedBy: labelManagedByValue, labelOwnerService: "compute.datumapis.com"},
			OwnerReferences: []metav1.OwnerReference{
				{UID: sc.UID, Name: sc.Name, Kind: "ServiceConfiguration", APIVersion: "services.miloapis.com/v1alpha1"},
			},
		},
		Spec: portalv1alpha1.ConsumerPortalPluginSpec{
			Slug: "compute-datumapis-com", DisplayName: "Compute",
			Assets:     portalv1alpha1.PluginAssets{BaseURL: "https://plugin.example.com"},
			Visibility: portalv1alpha1.PluginVisibility{Entitlement: portalv1alpha1.PluginEntitlementNone},
		},
	}
	ownedByOther := &portalv1alpha1.ConsumerPortalPlugin{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "other-service",
			Labels: map[string]string{labelManagedBy: labelManagedByValue, labelOwnerService: "other.datumapis.com"},
			OwnerReferences: []metav1.OwnerReference{
				{UID: types.UID("uid-other-sc"), Name: "other-config", Kind: "ServiceConfiguration", APIVersion: "services.miloapis.com/v1alpha1"},
			},
		},
		Spec: portalv1alpha1.ConsumerPortalPluginSpec{
			Slug: "other-service", DisplayName: "Other",
			Assets:     portalv1alpha1.PluginAssets{BaseURL: "https://other.example.com"},
			Visibility: portalv1alpha1.PluginVisibility{Entitlement: portalv1alpha1.PluginEntitlementNone},
		},
	}

	scheme := newUserInterfaceFanOutScheme()
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc, staleOwned, ownedByOther).Build()
	fanOut := &UserInterfaceFanOut{Client: base, Scheme: scheme}

	if err := fanOut.Reconcile(context.Background(), sc); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	err := base.Get(context.Background(), client.ObjectKey{Name: "compute-datumapis-com"}, &portalv1alpha1.ConsumerPortalPlugin{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected stale ConsumerPortalPlugin owned by this ServiceConfiguration to be pruned, got err=%v", err)
	}

	if err := base.Get(context.Background(), client.ObjectKey{Name: "other-service"}, &portalv1alpha1.ConsumerPortalPlugin{}); err != nil {
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
	consumer := &portalv1alpha1.ConsumerPortalPlugin{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "compute-datumapis-com",
			Labels:          map[string]string{labelManagedBy: labelManagedByValue, labelOwnerService: "compute.datumapis.com"},
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Spec: portalv1alpha1.ConsumerPortalPluginSpec{
			Slug: "compute-datumapis-com", DisplayName: "Compute",
			Assets:     portalv1alpha1.PluginAssets{BaseURL: "https://plugin.example.com"},
			Visibility: portalv1alpha1.PluginVisibility{Entitlement: portalv1alpha1.PluginEntitlementNone},
		},
	}
	provider := &portalv1alpha1.ProviderPortalPlugin{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "compute-datumapis-com",
			Labels:          map[string]string{labelManagedBy: labelManagedByValue, labelOwnerService: "compute.datumapis.com"},
			OwnerReferences: []metav1.OwnerReference{ownerRef},
		},
		Spec: portalv1alpha1.ProviderPortalPluginSpec{
			Slug: "compute-datumapis-com", DisplayName: "Compute",
			Assets: portalv1alpha1.PluginAssets{BaseURL: "https://plugin.example.com"},
		},
	}

	scheme := newUserInterfaceFanOutScheme()
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(consumer, provider).Build()
	fanOut := &UserInterfaceFanOut{Client: base, Scheme: scheme}

	if err := fanOut.Cleanup(context.Background(), sc); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if err := base.Get(context.Background(), client.ObjectKey{Name: "compute-datumapis-com"}, &portalv1alpha1.ConsumerPortalPlugin{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected ConsumerPortalPlugin to be deleted by Cleanup, got err=%v", err)
	}
	if err := base.Get(context.Background(), client.ObjectKey{Name: "compute-datumapis-com"}, &portalv1alpha1.ProviderPortalPlugin{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected ProviderPortalPlugin to be deleted by Cleanup, got err=%v", err)
	}
}
