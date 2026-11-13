package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
	quotav1alpha1 "go.miloapis.com/milo/pkg/apis/quota/v1alpha1"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

type failingPortalPatchClient struct {
	client.Client
	err error
}

func (patchClient *failingPortalPatchClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if resource, ok := obj.(*unstructured.Unstructured); ok && resource.GroupVersionKind().Group == portalGroupVersion.Group {
		return patchClient.err
	}
	return patchClient.Client.Patch(ctx, obj, patch, opts...)
}

type noopPortalPatchClient struct {
	client.Client
}

func (patchClient *noopPortalPatchClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if resource, ok := obj.(*unstructured.Unstructured); ok && resource.GroupVersionKind().Group == portalGroupVersion.Group {
		return nil
	}
	return patchClient.Client.Patch(ctx, obj, patch, opts...)
}

func newServiceConfigurationReconcilerScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = servicesv1alpha1.AddToScheme(scheme)
	_ = billingv1alpha1.AddToScheme(scheme)
	_ = quotav1alpha1.AddToScheme(scheme)
	return scheme
}

func TestServiceConfigurationReconcile_PortalFanOutFailureIsRetried(t *testing.T) {
	scheme := newServiceConfigurationReconcilerScheme()
	svc := newTestService("metal", "metal.datumapis.com", "Metal")
	sc := &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "metal",
			UID:        "uid-metal",
			Generation: 2,
			Finalizers: []string{serviceConfigurationFinalizer},
		},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef: servicesv1alpha1.ServiceReference{Name: "metal"},
			Phase:      servicesv1alpha1.PhasePublished,
			UserInterface: &servicesv1alpha1.UserInterfaceSpec{
				Consumer: &servicesv1alpha1.ConsumerUserInterfaceSpec{
					Assets:     basePluginAssets(),
					Visibility: servicesv1alpha1.PluginVisibility{Entitlement: "Required"},
				},
				Provider: &servicesv1alpha1.ProviderUserInterfaceSpec{Assets: basePluginAssets()},
			},
		},
	}
	base := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(svc, sc).
		WithStatusSubresource(&servicesv1alpha1.ServiceConfiguration{}).
		Build()

	applyErr := errors.New(`ConsumerPortalPlugin.portal.miloapis.com "metal-datumapis-com" is invalid: spec.serviceName: field not declared in schema`)
	reconciler := &ServiceConfigurationReconciler{
		Client:        base,
		Scheme:        scheme,
		BillingFanOut: &BillingFanOut{Client: base},
		QuotaFanOut: &QuotaFanOut{
			Client:     base,
			Scheme:     scheme,
			RESTMapper: apimeta.NewDefaultRESTMapper(nil),
		},
		UserInterfaceFanOut: &UserInterfaceFanOut{
			Client: &failingPortalPatchClient{Client: base, err: applyErr},
			Scheme: scheme,
		},
	}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "metal"}}

	_, err := reconciler.Reconcile(context.Background(), req)
	if err == nil {
		t.Fatal("Reconcile() returned nil for a failed portal plugin apply; controller-runtime would neither log nor retry it")
	}
	if !errors.Is(err, applyErr) || !strings.Contains(err.Error(), applyErr.Error()) {
		t.Fatalf("Reconcile() error = %v, want it to wrap the underlying apply error %v", err, applyErr)
	}

	var got servicesv1alpha1.ServiceConfiguration
	if err := base.Get(context.Background(), req.NamespacedName, &got); err != nil {
		t.Fatalf("get ServiceConfiguration: %v", err)
	}
	for _, expected := range []struct{ condType, reason string }{
		{ConditionTypeReady, reasonConsumerPortalFanOutFailed},
		{ConditionTypeConsumerPortalFanOutHealthy, reasonConsumerPortalFanOutFailed},
		{ConditionTypeProviderPortalFanOutHealthy, reasonProviderPortalFanOutFailed},
	} {
		cond := apimeta.FindStatusCondition(got.Status.Conditions, expected.condType)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != expected.reason {
			t.Errorf("condition %s = %+v, want False/%s (status must still be written before the error is returned)", expected.condType, cond, expected.reason)
		}
	}

	reconciler.UserInterfaceFanOut.Client = &noopPortalPatchClient{Client: base}
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("Reconcile() after the portal apply recovered = %v, want nil", err)
	}
	if err := base.Get(context.Background(), req.NamespacedName, &got); err != nil {
		t.Fatalf("get ServiceConfiguration: %v", err)
	}
	if ready := apimeta.FindStatusCondition(got.Status.Conditions, ConditionTypeReady); ready == nil || ready.Status != metav1.ConditionTrue {
		t.Errorf("Ready after recovery = %+v, want True", ready)
	}
}
