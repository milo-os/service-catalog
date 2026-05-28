// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// canManage mirrors the boolean the webhook derives from a SubjectAccessReview
// for the serviceconsumers.manage permission: true for the services controller,
// false for provider callers.
const (
	canManage    = true
	cannotManage = false
)

func newConsumer(name string, approval *servicesv1alpha1.ProviderApproval) *servicesv1alpha1.ServiceConsumer {
	return &servicesv1alpha1.ServiceConsumer{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: servicesv1alpha1.ServiceConsumerSpec{
			ServiceRef:         servicesv1alpha1.ServiceRef{Name: "compute"},
			ConsumerProjectRef: servicesv1alpha1.ConsumerProjectRef{Name: "consumer-proj"},
			Approval:           approval,
		},
	}
}

func TestValidateServiceConsumerCreate_AcceptsManager(t *testing.T) {
	errs := ValidateServiceConsumerCreate(canManage, newConsumer("sc-x", nil))
	if len(errs) != 0 {
		t.Fatalf("unexpected errors from manage-capable create: %v", errs)
	}
}

func TestValidateServiceConsumerCreate_RejectsNonManager(t *testing.T) {
	errs := ValidateServiceConsumerCreate(cannotManage, newConsumer("sc-x", nil))
	if len(errs) == 0 {
		t.Fatal("expected error when caller without manage creates ServiceConsumer")
	}
}

func TestValidateServiceConsumerUpdate_ProviderAccessOnApproval(t *testing.T) {
	oldSC := newConsumer("sc-x", nil)
	newSC := newConsumer("sc-x", &servicesv1alpha1.ProviderApproval{
		Decision: servicesv1alpha1.ApprovalDecisionApproved,
	})
	errs := ValidateServiceConsumerUpdate(cannotManage, oldSC, newSC)
	if len(errs) != 0 {
		t.Fatalf("provider update of spec.approval should be allowed, got %v", errs)
	}
}

func TestValidateServiceConsumerUpdate_ProviderRejectedOnServiceRef(t *testing.T) {
	oldSC := newConsumer("sc-x", nil)
	newSC := newConsumer("sc-x", nil)
	newSC.Spec.ServiceRef.Name = "other"
	errs := ValidateServiceConsumerUpdate(cannotManage, oldSC, newSC)
	if len(errs) == 0 {
		t.Fatal("expected error when provider mutates spec.serviceRef")
	}
}

func TestValidateServiceConsumerUpdate_ManagerAccessOnServiceRef(t *testing.T) {
	oldSC := newConsumer("sc-x", nil)
	newSC := newConsumer("sc-x", nil)
	newSC.Spec.ServiceRef.Name = "other"
	errs := ValidateServiceConsumerUpdate(canManage, oldSC, newSC)
	if len(errs) != 0 {
		t.Fatalf("manage-capable update of spec.serviceRef should be allowed, got %v", errs)
	}
}

func TestValidateServiceConsumerUpdate_DeniedImmutable(t *testing.T) {
	oldSC := newConsumer("sc-x", &servicesv1alpha1.ProviderApproval{
		Decision: servicesv1alpha1.ApprovalDecisionDenied,
	})
	newSC := newConsumer("sc-x", &servicesv1alpha1.ProviderApproval{
		Decision: servicesv1alpha1.ApprovalDecisionApproved,
	})
	errs := ValidateServiceConsumerUpdate(cannotManage, oldSC, newSC)
	if len(errs) == 0 {
		t.Fatal("expected error flipping Denied -> Approved")
	}
}

func TestValidateServiceConsumerUpdate_DeniedNoOp(t *testing.T) {
	// Denied -> Denied is a no-op and must be allowed.
	oldSC := newConsumer("sc-x", &servicesv1alpha1.ProviderApproval{
		Decision: servicesv1alpha1.ApprovalDecisionDenied,
	})
	newSC := newConsumer("sc-x", &servicesv1alpha1.ProviderApproval{
		Decision: servicesv1alpha1.ApprovalDecisionDenied,
	})
	errs := ValidateServiceConsumerUpdate(cannotManage, oldSC, newSC)
	if len(errs) != 0 {
		t.Fatalf("Denied -> Denied should be allowed, got %v", errs)
	}
}

func TestValidateServiceConsumerUpdate_FirstTimeDenied(t *testing.T) {
	// nil -> Denied is setting the decision for the first time and must be allowed.
	oldSC := newConsumer("sc-x", nil)
	newSC := newConsumer("sc-x", &servicesv1alpha1.ProviderApproval{
		Decision: servicesv1alpha1.ApprovalDecisionDenied,
	})
	errs := ValidateServiceConsumerUpdate(cannotManage, oldSC, newSC)
	if len(errs) != 0 {
		t.Fatalf("first-time Denied should be allowed, got %v", errs)
	}
}
