// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	"context"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

func TestRequesterFromUserInfo_HumanCaller(t *testing.T) {
	u := authenticationv1.UserInfo{UID: "user-abc123", Username: "alice@example.com"}

	got := requesterFromUserInfo(u)
	if got == nil {
		t.Fatal("expected a RequesterRef, got nil")
	}
	want := &servicesv1alpha1.RequesterRef{
		APIGroup: "iam.miloapis.com",
		Kind:     "User",
		Name:     "user-abc123",
		Email:    "alice@example.com",
	}
	if *got != *want {
		t.Errorf("requesterFromUserInfo() = %+v, want %+v", *got, *want)
	}
}

func TestRequesterFromUserInfo_NoUID(t *testing.T) {
	u := authenticationv1.UserInfo{Username: "alice@example.com"}
	if got := requesterFromUserInfo(u); got != nil {
		t.Errorf("requesterFromUserInfo() = %+v, want nil for a caller with no UID", got)
	}
}

func TestRequesterFromUserInfo_SystemCaller(t *testing.T) {
	for _, username := range []string{
		"system:serviceaccount:services-system:services-controller-manager",
		"system:apiserver",
	} {
		u := authenticationv1.UserInfo{UID: "some-uid", Username: username}
		if got := requesterFromUserInfo(u); got != nil {
			t.Errorf("requesterFromUserInfo(%q) = %+v, want nil for a system caller", username, got)
		}
	}
}

// TestDefault_NoAdmissionContext verifies Default is a no-op — not an error —
// when called outside an admission request, which envtest and direct calls
// against the fake client both do.
func TestDefault_NoAdmissionContext(t *testing.T) {
	w := &serviceEntitlementWebhook{}
	se := &servicesv1alpha1.ServiceEntitlement{
		ObjectMeta: metav1.ObjectMeta{Name: "a"},
		Spec:       servicesv1alpha1.ServiceEntitlementSpec{ServiceRef: servicesv1alpha1.ServiceRef{Name: "compute"}},
	}

	if err := w.Default(context.Background(), se); err != nil {
		t.Fatalf("Default() error = %v, want nil", err)
	}
	if se.Spec.RequestedBy != nil {
		t.Errorf("Default() set requestedBy = %+v without an admission request in context, want nil", se.Spec.RequestedBy)
	}
}

// TestDefault_LeavesExistingRequestedByAlone verifies Default never overwrites
// a value already present on the incoming object — the path
// ensureDependencies relies on to propagate a parent's requester onto a
// derived dependency entitlement.
func TestDefault_LeavesExistingRequestedByAlone(t *testing.T) {
	w := &serviceEntitlementWebhook{}
	existing := &servicesv1alpha1.RequesterRef{
		APIGroup: "iam.miloapis.com", Kind: "User", Name: "user-1", Email: "a@example.com",
	}
	se := &servicesv1alpha1.ServiceEntitlement{
		ObjectMeta: metav1.ObjectMeta{Name: "a"},
		Spec: servicesv1alpha1.ServiceEntitlementSpec{
			ServiceRef:  servicesv1alpha1.ServiceRef{Name: "compute"},
			RequestedBy: existing,
		},
	}

	if err := w.Default(context.Background(), se); err != nil {
		t.Fatalf("Default() error = %v, want nil", err)
	}
	if se.Spec.RequestedBy != existing {
		t.Errorf("Default() replaced an already-set requestedBy: got %+v, want the original %+v", se.Spec.RequestedBy, existing)
	}
}
