// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// The error IPAM returned on staging when its database was unreachable, which
// provisioning used to copy verbatim onto a customer-readable ledger.
const leakyDetail = "failed to begin transaction: failed to connect to `user=ipam database=ipam`: " +
	"10.2.6.188:5432 (ipam-postgres-pooler-rw): server error: FATAL: password authentication failed"

var ipClassResource = schema.GroupResource{Group: ipClassGVK.Group, Resource: "ipclasses"}

func assertNoLeak(t *testing.T, where, message string) {
	t.Helper()
	for _, secret := range []string{"10.2.6.188", "user=ipam", "postgres", "FATAL"} {
		if strings.Contains(message, secret) {
			t.Errorf("%s leaks %q: %q", where, secret, message)
		}
	}
}

func TestCustomerErrorDetail(t *testing.T) {
	invalid := apierrors.NewInvalid(schema.GroupKind{Group: ipClassGVK.Group, Kind: "IPClass"}, "x",
		field.ErrorList{field.Invalid(field.NewPath("spec", "source"), "y", "spec.source is immutable")})

	for _, tc := range []struct {
		name  string
		err   error
		shown bool
	}{
		{"internal", apierrors.NewInternalError(errors.New(leakyDetail)), false},
		{"wrapped internal", fmt.Errorf("apply: %w", apierrors.NewInternalError(errors.New(leakyDetail))), false},
		{"unavailable", apierrors.NewServiceUnavailable(leakyDetail), false},
		{"timeout", apierrors.NewTimeoutError(leakyDetail, 1), false},
		{"server timeout", apierrors.NewServerTimeout(ipClassResource, "create", 1), false},
		{"too many requests", apierrors.NewTooManyRequests(leakyDetail, 1), false},
		{"not an API error", errors.New(leakyDetail), false},
		{"invalid", invalid, true},
		{"wrapped invalid", fmt.Errorf("apply: %w", invalid), true},
		{"forbidden", apierrors.NewForbidden(ipClassResource, "x", errors.New("denied")), true},
		{"not found", apierrors.NewNotFound(ipClassResource, "x"), true},
		{"conflict", apierrors.NewConflict(ipClassResource, "x", errors.New("modified")), true},
		{"no match", &apimeta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "x", Kind: "Y"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			detail, shown := customerErrorDetail(tc.err)
			if shown != tc.shown {
				t.Fatalf("shown = %v, want %v", shown, tc.shown)
			}
			if !shown {
				if detail != "" {
					t.Errorf("withheld error returned detail %q", detail)
				}
				if got := customerErrorReason(context.Background(), tc.err); got != withheldErrorReason {
					t.Errorf("reason = %q, want the generic reason", got)
				}
			}
		})
	}
	if detail, _ := customerErrorDetail(invalid); !strings.Contains(detail, "spec.source is immutable") {
		t.Errorf("validation error lost its explanation: %q", detail)
	}
}

func provApplyFailing(err error) client.Client {
	return fake.NewClientBuilder().
		WithScheme(provScheme()).
		WithObjects(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive)).
		WithStatusSubresource(&servicesv1alpha1.ServiceEntitlement{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
				return err
			},
		}).
		Build()
}

// An internal error from the target API is a platform problem. The customer is
// told what could not be installed and that it will be retried, not how the
// target API's database is reached.
func TestProvisioningWithholdsInternalErrors(t *testing.T) {
	consumer := provApplyFailing(apierrors.NewInternalError(errors.New(leakyDetail)))
	root := provClient(provConfig(ipClassRef("datum-endpoint-ipv6")))
	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	ent := getEntitlement(t, consumer)
	entry := ent.Status.ProvisionedResources[0]
	if entry.State != servicesv1alpha1.ProvisionedResourceStateFailed || entry.Reason != reasonApplyFailed {
		t.Fatalf("expected Failed/ApplyFailed, got %+v", entry)
	}
	want := fmt.Sprintf(`%s could not install IPClass "datum-endpoint-ipv6": %s`, provServiceName, withheldErrorReason)
	if entry.Message != want {
		t.Errorf("ledger message = %q, want %q", entry.Message, want)
	}
	assertNoLeak(t, "ledger", entry.Message)
	cond := apimeta.FindStatusCondition(ent.Status.Conditions, servicesv1alpha1.ConditionTypeProvisioned)
	if cond == nil {
		t.Fatal("Provisioned condition missing")
	}
	assertNoLeak(t, "Provisioned condition", cond.Message)
}

// A rejection of the declared object explains what is wrong with it, and that
// stays readable.
func TestProvisioningKeepsValidationErrorsReadable(t *testing.T) {
	consumer := provApplyFailing(apierrors.NewInvalid(
		schema.GroupKind{Group: ipClassGVK.Group, Kind: "IPClass"}, "datum-endpoint-ipv6",
		field.ErrorList{field.Invalid(field.NewPath("spec", "source"), "datum-cloud", "spec.source is immutable")}))
	root := provClient(provConfig(ipClassRef("datum-endpoint-ipv6")))
	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	entry := getEntitlement(t, consumer).Status.ProvisionedResources[0]
	if !strings.Contains(entry.Message, "spec.source is immutable") {
		t.Errorf("validation error is no longer readable: %q", entry.Message)
	}
	if len(entry.Message) > maxLedgerMessageBytes {
		t.Errorf("ledger message is %d bytes, over the limit", len(entry.Message))
	}
}

// DependenciesSatisfied is read by project members too.
func TestDependenciesConditionWithholdsInternalErrors(t *testing.T) {
	parentSvc := newPublishedService(testServiceSlug, testServiceName, testProviderProject, "", testDepServiceSlug)
	depSvc := newPublishedService(testDepServiceSlug, "storage.miloapis.com", testProviderProject, "")
	parentEnt := newEntitlement(testServiceSlug, testServiceSlug)

	consumerClient := &failingEntitlementCreate{Client: newFakeClient(parentEnt)}

	mgr := newTestManager()
	mgr.add(testConsumerProject, consumerClient)
	mgr.add(testProviderProject, newFakeClient())
	r := &ServiceEntitlementReconciler{rootClient: newServiceRootClient(parentSvc, depSvc), Manager: mgr, Scheme: testScheme()}

	for i := 0; i < 3; i++ {
		_, _ = r.Reconcile(context.Background(), entitlementRequest(testConsumerProject, testServiceSlug))
	}

	var parent servicesv1alpha1.ServiceEntitlement
	if err := consumerClient.Get(context.Background(), types.NamespacedName{Name: testServiceSlug}, &parent); err != nil {
		t.Fatalf("get parent entitlement: %v", err)
	}
	cond := apimeta.FindStatusCondition(parent.Status.Conditions, servicesv1alpha1.ConditionTypeDependenciesSatisfied)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("DependenciesSatisfied = %v, want False", cond)
	}
	assertNoLeak(t, "DependenciesSatisfied", cond.Message)
	if !strings.Contains(cond.Message, testDepServiceSlug) || !strings.Contains(cond.Message, withheldErrorReason) {
		t.Errorf("condition should name the dependency and the generic reason: %q", cond.Message)
	}
}

// failingEntitlementCreate fails every ServiceEntitlement create the way an API
// server with a broken backend does.
type failingEntitlementCreate struct {
	client.Client
}

func (c *failingEntitlementCreate) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*servicesv1alpha1.ServiceEntitlement); ok {
		return apierrors.NewInternalError(errors.New(leakyDetail))
	}
	return c.Client.Create(ctx, obj, opts...)
}
