package activation

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

func TestNewServiceInfo(t *testing.T) {
	t.Run("maps every identity field", func(t *testing.T) {
		svc := &servicesv1alpha1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "compute"},
			Spec: servicesv1alpha1.ServiceSpec{
				ServiceName: "compute.datumapis.com",
				DisplayName: "Compute",
				Description: "Run workloads on Datum Cloud.",
				Phase:       servicesv1alpha1.PhasePublished,
				EnablementPolicy: &servicesv1alpha1.EnablementPolicy{
					Mode: servicesv1alpha1.EnablementModeGatedByProvider,
				},
			},
		}

		got := NewServiceInfo(svc)
		want := ServiceInfo{
			ObjectName:     "compute",
			CanonicalName:  "compute.datumapis.com",
			DisplayName:    "Compute",
			Description:    "Run workloads on Datum Cloud.",
			EnablementMode: servicesv1alpha1.EnablementModeGatedByProvider,
		}
		if got != want {
			t.Fatalf("NewServiceInfo() = %+v, want %+v", got, want)
		}
	})

	t.Run("nil EnablementPolicy defaults to SelfService", func(t *testing.T) {
		svc := &servicesv1alpha1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "compute"},
			Spec: servicesv1alpha1.ServiceSpec{
				ServiceName: "compute.datumapis.com",
				DisplayName: "Compute",
				Phase:       servicesv1alpha1.PhasePublished,
				// EnablementPolicy intentionally left nil.
			},
		}

		got := NewServiceInfo(svc)
		if got.EnablementMode != servicesv1alpha1.EnablementModeSelfService {
			t.Fatalf("NewServiceInfo().EnablementMode = %q, want %q (nil policy default)", got.EnablementMode, servicesv1alpha1.EnablementModeSelfService)
		}
	})
}

func TestConfigFromService(t *testing.T) {
	info := ServiceInfo{
		ObjectName:    "compute",
		CanonicalName: "compute.datumapis.com",
		DisplayName:   "Compute",
		Description:   "Run workloads on Datum Cloud.",
	}

	cfg := ConfigFromService(info)
	want := Config{
		ObjectName:    "compute",
		CanonicalName: "compute.datumapis.com",
		DisplayName:   "Compute",
	}
	if cfg != want {
		t.Fatalf("ConfigFromService() = %+v, want %+v", cfg, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("ConfigFromService() produced an invalid Config: %v", err)
	}
}
