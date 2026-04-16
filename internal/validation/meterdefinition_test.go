// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	servicesv1alpha1 "go.miloapis.com/services/api/v1alpha1"
)

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = servicesv1alpha1.AddToScheme(s)
	return s
}

func newMeterDefinition(name, meterName, owner string, dims ...string) *servicesv1alpha1.MeterDefinition {
	return &servicesv1alpha1.MeterDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: servicesv1alpha1.MeterDefinitionSpec{
			MeterName:   meterName,
			DisplayName: name,
			Owner:       servicesv1alpha1.MeterOwner{Service: owner},
			Measurement: servicesv1alpha1.MeterMeasurement{
				Aggregation: servicesv1alpha1.MeterAggregationSum,
				Unit:        "s",
				Dimensions:  dims,
			},
			Billing: servicesv1alpha1.MeterBilling{
				ConsumedUnit: "s",
				PricingUnit:  "h",
			},
		},
	}
}

func newServiceObj(name, serviceName string, phase servicesv1alpha1.Phase) *servicesv1alpha1.Service {
	return &servicesv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: servicesv1alpha1.ServiceSpec{
			ServiceName: serviceName,
			DisplayName: serviceName,
			Phase:       phase,
			Owner: servicesv1alpha1.ServiceOwner{
				ProducerProjectRef: servicesv1alpha1.ProducerProjectReference{Name: "p"},
			},
		},
	}
}

// fakeClientWithIndexers builds a fake client with the indexers wired
// so validation can call opts.Client.List with MatchingFields.
func fakeClientWithIndexers(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(objs...).
		WithIndex(&servicesv1alpha1.MeterDefinition{}, meterDefinitionMeterNameFieldKey,
			func(obj client.Object) []string {
				md := obj.(*servicesv1alpha1.MeterDefinition)
				return []string{md.Spec.MeterName}
			}).
		WithIndex(&servicesv1alpha1.Service{}, serviceServiceNameFieldKey,
			func(obj client.Object) []string {
				svc := obj.(*servicesv1alpha1.Service)
				return []string{svc.Spec.ServiceName}
			}).
		Build()
}

func TestValidateMeterNamePrefix(t *testing.T) {
	tests := []struct {
		name      string
		meterName string
		owner     string
		wantErr   bool
	}{
		{"valid prefix", "compute.miloapis.com/instance/cpu-seconds", "compute.miloapis.com", false},
		{"missing owner prefix", "something/else", "compute.miloapis.com", true},
		{"no path segment", "compute.miloapis.com/", "compute.miloapis.com", true},
		{"wrong owner", "storage.miloapis.com/bucket/ops", "compute.miloapis.com", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			md := newMeterDefinition("m", tt.meterName, tt.owner)
			errs := validateMeterNamePrefix(md)
			if (len(errs) > 0) != tt.wantErr {
				t.Errorf("validateMeterNamePrefix() errs = %v, wantErr %v", errs, tt.wantErr)
			}
		})
	}
}

func TestValidateMeterUnits(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(md *servicesv1alpha1.MeterDefinition)
		wantErr bool
	}{
		{"all units set", func(md *servicesv1alpha1.MeterDefinition) {}, false},
		{"empty measurement unit", func(md *servicesv1alpha1.MeterDefinition) { md.Spec.Measurement.Unit = "" }, true},
		{"whitespace pricing unit", func(md *servicesv1alpha1.MeterDefinition) { md.Spec.Billing.PricingUnit = "h r" }, true},
		{"empty consumed unit", func(md *servicesv1alpha1.MeterDefinition) { md.Spec.Billing.ConsumedUnit = "" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			md := newMeterDefinition("m", "svc/m", "svc")
			tt.mutate(md)
			errs := validateMeterUnits(md)
			if (len(errs) > 0) != tt.wantErr {
				t.Errorf("validateMeterUnits() errs = %v, wantErr %v", errs, tt.wantErr)
			}
		})
	}
}

func TestValidateMeterDefinitionCreate_OwnerServiceExists(t *testing.T) {
	svc := newServiceObj("compute", "compute.miloapis.com", servicesv1alpha1.PhasePublished)
	cl := fakeClientWithIndexers(svc)

	md := newMeterDefinition("m1", "compute.miloapis.com/instance/cpu-seconds", "compute.miloapis.com")
	opts := MeterDefinitionValidationOptions{Context: context.Background(), Client: cl}

	errs := ValidateMeterDefinitionCreate(md, opts)
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}

	// Missing owning service.
	emptyCl := fakeClientWithIndexers()
	errs = ValidateMeterDefinitionCreate(md, MeterDefinitionValidationOptions{
		Context: context.Background(), Client: emptyCl,
	})
	if len(errs) == 0 {
		t.Fatalf("expected error when owning service is missing, got none")
	}
}

func TestValidateMeterDefinitionCreate_DuplicateMeterName(t *testing.T) {
	svc := newServiceObj("compute", "compute.miloapis.com", servicesv1alpha1.PhasePublished)
	existing := newMeterDefinition("m-a", "compute.miloapis.com/instance/cpu-seconds", "compute.miloapis.com")
	existing.UID = "existing-uid"
	cl := fakeClientWithIndexers(svc, existing)

	candidate := newMeterDefinition("m-b", "compute.miloapis.com/instance/cpu-seconds", "compute.miloapis.com")
	candidate.UID = "candidate-uid"
	errs := ValidateMeterDefinitionCreate(candidate, MeterDefinitionValidationOptions{
		Context: context.Background(), Client: cl,
	})
	if len(errs) == 0 {
		t.Fatalf("expected duplicate meterName error, got none")
	}
}

func TestValidateMeterDefinitionUpdate_Immutability(t *testing.T) {
	svc := newServiceObj("compute", "compute.miloapis.com", servicesv1alpha1.PhasePublished)
	cl := fakeClientWithIndexers(svc)
	opts := MeterDefinitionValidationOptions{Context: context.Background(), Client: cl}

	oldMD := newMeterDefinition("m", "compute.miloapis.com/instance/cpu-seconds", "compute.miloapis.com", "region")
	tests := []struct {
		name    string
		mutate  func(md *servicesv1alpha1.MeterDefinition)
		wantErr bool
	}{
		{"no changes", func(md *servicesv1alpha1.MeterDefinition) {}, false},
		{"change meterName", func(md *servicesv1alpha1.MeterDefinition) {
			md.Spec.MeterName = "compute.miloapis.com/other"
		}, true},
		{"change owner", func(md *servicesv1alpha1.MeterDefinition) { md.Spec.Owner.Service = "storage.miloapis.com" }, true},
		{"change unit", func(md *servicesv1alpha1.MeterDefinition) { md.Spec.Measurement.Unit = "By" }, true},
		{"remove dimension", func(md *servicesv1alpha1.MeterDefinition) {
			md.Spec.Measurement.Dimensions = nil
		}, true},
		{"add dimension", func(md *servicesv1alpha1.MeterDefinition) {
			md.Spec.Measurement.Dimensions = []string{"region", "tier"}
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newMD := oldMD.DeepCopy()
			tt.mutate(newMD)
			errs := ValidateMeterDefinitionUpdate(oldMD, newMD, opts)
			if (len(errs) > 0) != tt.wantErr {
				t.Errorf("ValidateMeterDefinitionUpdate() errs = %v, wantErr %v", errs, tt.wantErr)
			}
		})
	}
}

func TestValidateMeterDefinitionUpdate_PhaseTransitions(t *testing.T) {
	svc := newServiceObj("compute", "compute.miloapis.com", servicesv1alpha1.PhasePublished)
	cl := fakeClientWithIndexers(svc)
	opts := MeterDefinitionValidationOptions{Context: context.Background(), Client: cl}

	tests := []struct {
		name    string
		from    servicesv1alpha1.Phase
		to      servicesv1alpha1.Phase
		wantErr bool
	}{
		{"forward draft->published", servicesv1alpha1.PhaseDraft, servicesv1alpha1.PhasePublished, false},
		{"forward published->deprecated", servicesv1alpha1.PhasePublished, servicesv1alpha1.PhaseDeprecated, false},
		{"backward published->draft", servicesv1alpha1.PhasePublished, servicesv1alpha1.PhaseDraft, true},
		{"skip draft->retired", servicesv1alpha1.PhaseDraft, servicesv1alpha1.PhaseRetired, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldMD := newMeterDefinition("m", "compute.miloapis.com/instance/cpu-seconds", "compute.miloapis.com")
			oldMD.Spec.Phase = tt.from
			newMD := oldMD.DeepCopy()
			newMD.Spec.Phase = tt.to

			errs := ValidateMeterDefinitionUpdate(oldMD, newMD, opts)
			if (len(errs) > 0) != tt.wantErr {
				t.Errorf("ValidateMeterDefinitionUpdate() errs = %v, wantErr %v", errs, tt.wantErr)
			}
		})
	}
}
