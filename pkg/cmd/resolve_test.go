package cmd

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

func publishedService(objectName, canonicalName, displayName string) servicesv1alpha1.Service {
	return servicesv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: objectName},
		Spec: servicesv1alpha1.ServiceSpec{
			ServiceName: canonicalName,
			DisplayName: displayName,
			Phase:       servicesv1alpha1.PhasePublished,
		},
	}
}

func draftService(objectName, canonicalName, displayName string) servicesv1alpha1.Service {
	svc := publishedService(objectName, canonicalName, displayName)
	svc.Spec.Phase = servicesv1alpha1.PhaseDraft
	return svc
}

func TestResolveService(t *testing.T) {
	compute := publishedService("compute", "compute.datumapis.com", "Compute")
	network := publishedService("net-svc", "networking.datumapis.com", "Networking")
	draft := draftService("preview", "preview.datumapis.com", "Preview")

	list := &servicesv1alpha1.ServiceList{Items: []servicesv1alpha1.Service{compute, network, draft}}

	t.Run("matches canonical name", func(t *testing.T) {
		info, err := resolveService(list, "compute.datumapis.com")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if info.ObjectName != "compute" {
			t.Fatalf("got ObjectName %q, want %q", info.ObjectName, "compute")
		}
	})

	t.Run("falls back to object name", func(t *testing.T) {
		info, err := resolveService(list, "net-svc")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if info.CanonicalName != "networking.datumapis.com" {
			t.Fatalf("got CanonicalName %q, want %q", info.CanonicalName, "networking.datumapis.com")
		}
	})

	t.Run("prefers canonical match over another service's object-name coincidence", func(t *testing.T) {
		// A service whose ObjectName equals another service's CanonicalName
		// must not shadow the exact canonical match.
		decoy := publishedService("compute.datumapis.com", "decoy.datumapis.com", "Decoy")
		listWithDecoy := &servicesv1alpha1.ServiceList{Items: []servicesv1alpha1.Service{decoy, compute}}

		info, err := resolveService(listWithDecoy, "compute.datumapis.com")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if info.ObjectName != "compute" {
			t.Fatalf("got ObjectName %q, want the canonical match %q", info.ObjectName, "compute")
		}
	})

	t.Run("ignores unpublished services", func(t *testing.T) {
		if _, err := resolveService(list, "preview.datumapis.com"); err == nil {
			t.Fatal("expected an error for a draft (unpublished) service, got nil")
		}
		if _, err := resolveService(list, "preview"); err == nil {
			t.Fatal("expected an error for a draft (unpublished) service, got nil")
		}
	})

	t.Run("not found error suggests services list", func(t *testing.T) {
		_, err := resolveService(list, "does-not-exist")
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		if !strings.Contains(err.Error(), "datumctl services list") {
			t.Fatalf("error %q does not mention the list command", err.Error())
		}
	})

	t.Run("nil list", func(t *testing.T) {
		_, err := resolveService(nil, "compute")
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
	})
}
