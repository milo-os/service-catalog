// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	servicesv1alpha1 "go.miloapis.com/services/api/v1alpha1"
)

func newService(name, serviceName string) *servicesv1alpha1.Service {
	return &servicesv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: servicesv1alpha1.ServiceSpec{
			ServiceName: serviceName,
			DisplayName: "Example",
			Owner: servicesv1alpha1.ServiceOwner{
				ProducerProjectRef: servicesv1alpha1.ProducerProjectReference{
					Name: "example-project",
				},
			},
		},
	}
}

func TestValidateServiceCreate(t *testing.T) {
	tests := []struct {
		name    string
		svc     *servicesv1alpha1.Service
		wantErr bool
	}{
		{
			name:    "valid slug and reverse-DNS serviceName",
			svc:     newService("compute-registry", "compute.miloapis.com"),
			wantErr: false,
		},
		{
			name:    "invalid metadata.name with uppercase",
			svc:     newService("Compute-Registry", "compute.miloapis.com"),
			wantErr: true,
		},
		{
			name:    "invalid metadata.name starts with hyphen",
			svc:     newService("-compute", "compute.miloapis.com"),
			wantErr: true,
		},
		{
			name:    "invalid serviceName not reverse-DNS",
			svc:     newService("compute-registry", "compute"),
			wantErr: true,
		},
		{
			name:    "invalid serviceName uppercase",
			svc:     newService("compute-registry", "Compute.MiloApis.com"),
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := ValidateServiceCreate(tt.svc)
			if (len(errs) > 0) != tt.wantErr {
				t.Errorf("ValidateServiceCreate() errs = %v, wantErr %v", errs, tt.wantErr)
			}
		})
	}
}

func TestValidateServiceUpdate_ServiceNameImmutable(t *testing.T) {
	oldSvc := newService("compute-registry", "compute.miloapis.com")
	newSvc := newService("compute-registry", "compute-v2.miloapis.com")
	errs := ValidateServiceUpdate(oldSvc, newSvc)
	if len(errs) == 0 {
		t.Fatalf("expected serviceName immutability error, got none")
	}

	// Unchanged update should be accepted.
	unchanged := ValidateServiceUpdate(oldSvc, oldSvc.DeepCopy())
	if len(unchanged) != 0 {
		t.Fatalf("expected no errors for unchanged update, got %v", unchanged)
	}
}

func TestValidateServiceUpdate_PhaseTransitions(t *testing.T) {
	tests := []struct {
		name    string
		from    servicesv1alpha1.Phase
		to      servicesv1alpha1.Phase
		wantErr bool
	}{
		{"forward draft->published", servicesv1alpha1.PhaseDraft, servicesv1alpha1.PhasePublished, false},
		{"forward published->deprecated", servicesv1alpha1.PhasePublished, servicesv1alpha1.PhaseDeprecated, false},
		{"forward deprecated->retired", servicesv1alpha1.PhaseDeprecated, servicesv1alpha1.PhaseRetired, false},
		{"noop retired->retired", servicesv1alpha1.PhaseRetired, servicesv1alpha1.PhaseRetired, false},
		{"backward published->draft", servicesv1alpha1.PhasePublished, servicesv1alpha1.PhaseDraft, true},
		{"backward deprecated->published", servicesv1alpha1.PhaseDeprecated, servicesv1alpha1.PhasePublished, true},
		{"skip draft->deprecated", servicesv1alpha1.PhaseDraft, servicesv1alpha1.PhaseDeprecated, true},
		{"skip draft->retired", servicesv1alpha1.PhaseDraft, servicesv1alpha1.PhaseRetired, true},
		{"skip published->retired", servicesv1alpha1.PhasePublished, servicesv1alpha1.PhaseRetired, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldSvc := newService("compute-registry", "compute.miloapis.com")
			oldSvc.Spec.Phase = tt.from
			newSvc := oldSvc.DeepCopy()
			newSvc.Spec.Phase = tt.to

			errs := ValidateServiceUpdate(oldSvc, newSvc)
			if (len(errs) > 0) != tt.wantErr {
				t.Errorf("ValidateServiceUpdate() errs = %v, wantErr %v", errs, tt.wantErr)
			}
		})
	}
}
