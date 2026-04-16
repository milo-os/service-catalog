// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	servicesv1alpha1 "go.miloapis.com/services/api/v1alpha1"
)

func newMRT(
	name, resourceTypeName, owner, group, kind string,
	labels ...servicesv1alpha1.MonitoredResourceLabel,
) *servicesv1alpha1.MonitoredResourceType {
	return &servicesv1alpha1.MonitoredResourceType{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: servicesv1alpha1.MonitoredResourceTypeSpec{
			ResourceTypeName: resourceTypeName,
			DisplayName:      name,
			Owner:            servicesv1alpha1.MonitoredResourceTypeOwner{Service: owner},
			GVK: servicesv1alpha1.MonitoredResourceTypeGVK{
				Group: group,
				Kind:  kind,
			},
			Labels: labels,
		},
	}
}

func TestValidateResourceTypeNameConsistency(t *testing.T) {
	tests := []struct {
		name             string
		resourceTypeName string
		group            string
		kind             string
		wantErr          bool
	}{
		{"matches", "compute.miloapis.com/Instance", "compute.miloapis.com", "Instance", false},
		{"mismatched", "compute.miloapis.com/Pod", "compute.miloapis.com", "Instance", true},
		{"empty group skipped", "compute.miloapis.com/Instance", "", "Instance", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mrt := newMRT("m", tt.resourceTypeName, tt.group, tt.group, tt.kind)
			errs := validateResourceTypeNameConsistency(mrt)
			if (len(errs) > 0) != tt.wantErr {
				t.Errorf("validateResourceTypeNameConsistency() errs = %v, wantErr %v", errs, tt.wantErr)
			}
		})
	}
}

func TestValidateOwnerMatchesGroup(t *testing.T) {
	tests := []struct {
		name    string
		owner   string
		group   string
		wantErr bool
	}{
		{"matches", "compute.miloapis.com", "compute.miloapis.com", false},
		{"mismatch", "storage.miloapis.com", "compute.miloapis.com", true},
		{"empty owner skipped", "", "compute.miloapis.com", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mrt := newMRT("m", "irrelevant", tt.owner, tt.group, "Instance")
			errs := validateOwnerMatchesGroup(mrt)
			if (len(errs) > 0) != tt.wantErr {
				t.Errorf("validateOwnerMatchesGroup() errs = %v, wantErr %v", errs, tt.wantErr)
			}
		})
	}
}

func TestValidateMonitoredResourceLabels(t *testing.T) {
	tests := []struct {
		name    string
		labels  []servicesv1alpha1.MonitoredResourceLabel
		wantErr bool
	}{
		{"empty", nil, false},
		{"valid single", []servicesv1alpha1.MonitoredResourceLabel{{Name: "region"}}, false},
		{"duplicate", []servicesv1alpha1.MonitoredResourceLabel{
			{Name: "region"}, {Name: "region"},
		}, true},
		{"invalid shape", []servicesv1alpha1.MonitoredResourceLabel{
			{Name: "Region"},
		}, true},
		{"invalid empty name", []servicesv1alpha1.MonitoredResourceLabel{
			{Name: ""},
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mrt := newMRT("m", "g/K", "g", "g", "K", tt.labels...)
			errs := validateMonitoredResourceLabels(mrt)
			if (len(errs) > 0) != tt.wantErr {
				t.Errorf("validateMonitoredResourceLabels() errs = %v, wantErr %v", errs, tt.wantErr)
			}
		})
	}
}

func TestValidateMonitoredResourceTypeCreate_OwnerServiceExists(t *testing.T) {
	svc := newServiceObj("compute", "compute.miloapis.com", servicesv1alpha1.PhasePublished)
	cl := fakeClientWithIndexers(svc)
	mrt := newMRT(
		"compute-instance",
		"compute.miloapis.com/Instance",
		"compute.miloapis.com",
		"compute.miloapis.com",
		"Instance",
	)
	errs := ValidateMonitoredResourceTypeCreate(mrt, MonitoredResourceTypeValidationOptions{
		Context: context.Background(), Client: cl,
	})
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}

	// Without matching Service, expect an error.
	emptyCl := fakeClientWithIndexers()
	errs = ValidateMonitoredResourceTypeCreate(mrt, MonitoredResourceTypeValidationOptions{
		Context: context.Background(), Client: emptyCl,
	})
	if len(errs) == 0 {
		t.Fatalf("expected error when owning service is missing, got none")
	}
}

func TestValidateMonitoredResourceTypeUpdate_Immutability(t *testing.T) {
	svc := newServiceObj("compute", "compute.miloapis.com", servicesv1alpha1.PhasePublished)
	cl := fakeClientWithIndexers(svc)
	opts := MonitoredResourceTypeValidationOptions{Context: context.Background(), Client: cl}

	oldMRT := newMRT(
		"compute-instance",
		"compute.miloapis.com/Instance",
		"compute.miloapis.com",
		"compute.miloapis.com",
		"Instance",
	)
	tests := []struct {
		name    string
		mutate  func(mrt *servicesv1alpha1.MonitoredResourceType)
		wantErr bool
	}{
		{"no changes", func(mrt *servicesv1alpha1.MonitoredResourceType) {}, false},
		{"change resourceTypeName", func(mrt *servicesv1alpha1.MonitoredResourceType) {
			mrt.Spec.ResourceTypeName = "compute.miloapis.com/Pod"
			mrt.Spec.GVK.Kind = "Pod"
		}, true},
		{"change gvk.kind", func(mrt *servicesv1alpha1.MonitoredResourceType) {
			mrt.Spec.GVK.Kind = "Pod"
		}, true},
		{"change gvk.group", func(mrt *servicesv1alpha1.MonitoredResourceType) {
			mrt.Spec.GVK.Group = "storage.miloapis.com"
		}, true},
		{"add label", func(mrt *servicesv1alpha1.MonitoredResourceType) {
			mrt.Spec.Labels = append(mrt.Spec.Labels, servicesv1alpha1.MonitoredResourceLabel{Name: "tier"})
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newMRT := oldMRT.DeepCopy()
			tt.mutate(newMRT)
			errs := ValidateMonitoredResourceTypeUpdate(oldMRT, newMRT, opts)
			if (len(errs) > 0) != tt.wantErr {
				t.Errorf("ValidateMonitoredResourceTypeUpdate() errs = %v, wantErr %v", errs, tt.wantErr)
			}
		})
	}
}

func TestValidateMonitoredResourceTypeUpdate_PhaseTransitions(t *testing.T) {
	svc := newServiceObj("compute", "compute.miloapis.com", servicesv1alpha1.PhasePublished)
	cl := fakeClientWithIndexers(svc)
	opts := MonitoredResourceTypeValidationOptions{Context: context.Background(), Client: cl}

	tests := []struct {
		name    string
		from    servicesv1alpha1.Phase
		to      servicesv1alpha1.Phase
		wantErr bool
	}{
		{"forward draft->published", servicesv1alpha1.PhaseDraft, servicesv1alpha1.PhasePublished, false},
		{"forward deprecated->retired", servicesv1alpha1.PhaseDeprecated, servicesv1alpha1.PhaseRetired, false},
		{"backward deprecated->published", servicesv1alpha1.PhaseDeprecated, servicesv1alpha1.PhasePublished, true},
		{"skip draft->retired", servicesv1alpha1.PhaseDraft, servicesv1alpha1.PhaseRetired, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldMRT := newMRT(
				"compute-instance",
				"compute.miloapis.com/Instance",
				"compute.miloapis.com",
				"compute.miloapis.com",
				"Instance",
			)
			oldMRT.Spec.Phase = tt.from
			newMRT := oldMRT.DeepCopy()
			newMRT.Spec.Phase = tt.to

			errs := ValidateMonitoredResourceTypeUpdate(oldMRT, newMRT, opts)
			if (len(errs) > 0) != tt.wantErr {
				t.Errorf("ValidateMonitoredResourceTypeUpdate() errs = %v, wantErr %v", errs, tt.wantErr)
			}
		})
	}
}
