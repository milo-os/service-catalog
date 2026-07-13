package activation

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// testConfig mirrors the compute plugin's configuration.
var testConfig = Config{
	ObjectName:    "compute",
	CanonicalName: "compute.datumapis.com",
	DisplayName:   "Compute",
}

// reasonConsumerDenied is a transient ServiceConsumer-relay Ready reason no
// state may key on; unlike the ReasonEntitlement* reasons it is not part of
// the ServiceEntitlement API surface, so there is no exported constant to
// reference.
const reasonConsumerDenied = "ConsumerDenied"

// entitlement builds a ServiceEntitlement with a Ready condition, matching the
// controller's setEntitlementStatus write path.
func entitlement(spec string, phase servicesv1alpha1.EntitlementPhase, reason, message string, entitledAt *metav1.Time) *servicesv1alpha1.ServiceEntitlement {
	e := &servicesv1alpha1.ServiceEntitlement{
		ObjectMeta: metav1.ObjectMeta{Name: "compute"},
		Spec:       servicesv1alpha1.ServiceEntitlementSpec{ServiceRef: servicesv1alpha1.ServiceRef{Name: spec}},
		Status: servicesv1alpha1.ServiceEntitlementStatus{
			Phase:      phase,
			EntitledAt: entitledAt,
		},
	}
	status := metav1.ConditionFalse
	if phase == servicesv1alpha1.EntitlementPhaseActive {
		status = metav1.ConditionTrue
	}
	e.Status.Conditions = []metav1.Condition{{
		Type:    servicesv1alpha1.ConditionTypeReady,
		Status:  status,
		Reason:  reason,
		Message: message,
	}}
	return e
}

// service builds a Service with the given phase, object name, canonical name,
// and display name. Enablement policy is left nil (defaults to SelfService).
func service(objectName, canonicalName, displayName string, phase servicesv1alpha1.Phase) *servicesv1alpha1.Service {
	return &servicesv1alpha1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: objectName},
		Spec: servicesv1alpha1.ServiceSpec{
			ServiceName: canonicalName,
			DisplayName: displayName,
			Phase:       phase,
		},
	}
}

func TestClassify(t *testing.T) {
	now := metav1.Now()

	tests := []struct {
		name string
		obs  Observation
		want State
	}{
		{
			name: "catalog API group absent",
			obs:  Observation{CatalogAbsent: true},
			want: StateCatalogUnavailable,
		},
		{
			name: "no entitlement found",
			obs:  Observation{Entitlement: nil},
			want: StateNotRequested,
		},
		{
			name: "created but no status yet",
			obs: Observation{Entitlement: &servicesv1alpha1.ServiceEntitlement{
				ObjectMeta: metav1.ObjectMeta{Name: "compute"},
				Spec:       servicesv1alpha1.ServiceEntitlementSpec{ServiceRef: servicesv1alpha1.ServiceRef{Name: "compute"}},
			}},
			want: StateProcessing,
		},
		{
			name: "phase set but Ready condition not yet written",
			obs: Observation{Entitlement: &servicesv1alpha1.ServiceEntitlement{
				ObjectMeta: metav1.ObjectMeta{Name: "compute"},
				Status:     servicesv1alpha1.ServiceEntitlementStatus{Phase: servicesv1alpha1.EntitlementPhasePendingApproval},
			}},
			want: StateProcessing,
		},
		{
			name: "active",
			obs: Observation{Entitlement: entitlement("compute", servicesv1alpha1.EntitlementPhaseActive,
				servicesv1alpha1.ReasonEntitlementActive, "This service is enabled and ready to use.", &now)},
			want: StateActive,
		},
		{
			name: "pending approval",
			obs: Observation{Entitlement: entitlement("compute", servicesv1alpha1.EntitlementPhasePendingApproval,
				servicesv1alpha1.ReasonEntitlementPendingApproval, "Waiting for the service provider to approve this request.", nil)},
			want: StatePendingApproval,
		},
		{
			name: "denied: rejected with entitledAt unset",
			obs: Observation{Entitlement: entitlement("compute", servicesv1alpha1.EntitlementPhaseRejected,
				servicesv1alpha1.ReasonEntitlementRejected, "The service provider denied this request.", nil)},
			want: StateDenied,
		},
		{
			name: "revoked: rejected with entitledAt set",
			obs: Observation{Entitlement: entitlement("compute", servicesv1alpha1.EntitlementPhaseRejected,
				servicesv1alpha1.ReasonEntitlementRejected, "The service provider denied this request.", &now)},
			want: StateRevoked,
		},
		{
			name: "revoked keys on entitledAt even when reason is the transient ConsumerDenied relay",
			obs: Observation{Entitlement: entitlement("compute", servicesv1alpha1.EntitlementPhaseRejected,
				reasonConsumerDenied, "The service provider denied this request.", &now)},
			want: StateRevoked,
		},
		{
			name: "unavailable: rejected with ServiceNotPublished (service missing)",
			obs: Observation{Entitlement: entitlement("compute", servicesv1alpha1.EntitlementPhaseRejected,
				servicesv1alpha1.ReasonServiceNotPublished, "The requested service could not be found.", nil)},
			want: StateUnavailable,
		},
		{
			name: "unavailable takes precedence over the revoked split even when entitledAt is set",
			obs: Observation{Entitlement: entitlement("compute", servicesv1alpha1.EntitlementPhaseRejected,
				servicesv1alpha1.ReasonServiceNotPublished, "The service \"compute.datumapis.com\" isn't published yet, so it can't be enabled.", &now)},
			want: StateUnavailable,
		},
		{
			name: "active classification ignores a transient relay reason",
			obs: Observation{Entitlement: entitlement("compute", servicesv1alpha1.EntitlementPhaseActive,
				"ConsumerApproved", "This service is enabled and ready to use.", &now)},
			want: StateActive,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.obs); got != tc.want {
				t.Fatalf("Classify() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSelectEntitlement(t *testing.T) {
	direct := entitlement("compute", servicesv1alpha1.EntitlementPhaseActive, servicesv1alpha1.ReasonEntitlementActive, "ok", nil)
	direct.Name = "compute"
	canonical := entitlement("compute.datumapis.com", servicesv1alpha1.EntitlementPhasePendingApproval, servicesv1alpha1.ReasonEntitlementPendingApproval, "waiting", nil)
	canonical.Name = "compute.datumapis.com"
	other := entitlement("network", servicesv1alpha1.EntitlementPhaseActive, servicesv1alpha1.ReasonEntitlementActive, "ok", nil)
	other.Name = "network"

	t.Run("object-name match when only the direct entitlement exists", func(t *testing.T) {
		list := &servicesv1alpha1.ServiceEntitlementList{Items: []servicesv1alpha1.ServiceEntitlement{*other, *direct}}
		got := SelectEntitlement(list, testConfig)
		if got == nil || got.Spec.ServiceRef.Name != "compute" {
			t.Fatalf("SelectEntitlement() = %+v, want the compute (object-name) entitlement", got)
		}
	})

	t.Run("canonical-name match is preferred over the object-name fallback", func(t *testing.T) {
		list := &servicesv1alpha1.ServiceEntitlementList{Items: []servicesv1alpha1.ServiceEntitlement{*direct, *canonical}}
		got := SelectEntitlement(list, testConfig)
		if got == nil || got.Spec.ServiceRef.Name != "compute.datumapis.com" {
			t.Fatalf("SelectEntitlement() = %+v, want the canonical-name entitlement", got)
		}
	})

	t.Run("no match returns nil", func(t *testing.T) {
		list := &servicesv1alpha1.ServiceEntitlementList{Items: []servicesv1alpha1.ServiceEntitlement{*other}}
		if got := SelectEntitlement(list, testConfig); got != nil {
			t.Fatalf("SelectEntitlement() = %+v, want nil", got)
		}
	})

	t.Run("status.serviceName is preferred over the spec object-name reference", func(t *testing.T) {
		// spec.serviceRef.name is the k8s object name ("compute"); the controller
		// resolves and stamps the canonical reverse-DNS identifier onto
		// status.serviceName once it reconciles. Canonical selection must key on
		// the stamped status, not assume the two strings coincide.
		stamped := entitlement("compute", servicesv1alpha1.EntitlementPhaseActive, servicesv1alpha1.ReasonEntitlementActive, "ok", nil)
		stamped.Name = "compute"
		stamped.Status.ServiceName = "compute.datumapis.com"

		list := &servicesv1alpha1.ServiceEntitlementList{Items: []servicesv1alpha1.ServiceEntitlement{*other, *stamped}}
		got := SelectEntitlement(list, testConfig)
		if got == nil || got.Name != "compute" {
			t.Fatalf("SelectEntitlement() = %+v, want the entitlement whose status.serviceName is canonical", got)
		}
	})
}

func TestJoinCatalog(t *testing.T) {
	t.Run("Draft services are filtered out", func(t *testing.T) {
		draft := service("staging-feature", "staging-feature.datumapis.com", "Staging Feature", servicesv1alpha1.PhaseDraft)
		published := service("compute", "compute.datumapis.com", "Compute", servicesv1alpha1.PhasePublished)

		services := &servicesv1alpha1.ServiceList{Items: []servicesv1alpha1.Service{*draft, *published}}
		entries := JoinCatalog(services, &servicesv1alpha1.ServiceEntitlementList{})

		if len(entries) != 1 {
			t.Fatalf("JoinCatalog() returned %d entries, want 1 (Draft filtered out): %+v", len(entries), entries)
		}
		if entries[0].Service.ObjectName != "compute" {
			t.Fatalf("JoinCatalog() kept %+v, want the Published compute service", entries[0].Service)
		}
	})

	t.Run("canonical-name match is preferred over the object-name fallback", func(t *testing.T) {
		compute := service("compute", "compute.datumapis.com", "Compute", servicesv1alpha1.PhasePublished)
		direct := entitlement("compute", servicesv1alpha1.EntitlementPhaseActive, servicesv1alpha1.ReasonEntitlementActive, "ok", nil)
		direct.Name = "compute"
		canonical := entitlement("compute.datumapis.com", servicesv1alpha1.EntitlementPhasePendingApproval, servicesv1alpha1.ReasonEntitlementPendingApproval, "waiting", nil)
		canonical.Name = "compute.datumapis.com"

		services := &servicesv1alpha1.ServiceList{Items: []servicesv1alpha1.Service{*compute}}
		entitlements := &servicesv1alpha1.ServiceEntitlementList{Items: []servicesv1alpha1.ServiceEntitlement{*direct, *canonical}}

		entries := JoinCatalog(services, entitlements)
		if len(entries) != 1 {
			t.Fatalf("JoinCatalog() returned %d entries, want 1", len(entries))
		}
		if entries[0].Entitlement == nil || entries[0].Entitlement.Name != "compute.datumapis.com" {
			t.Fatalf("JoinCatalog() matched %+v, want the canonical-name entitlement", entries[0].Entitlement)
		}
		if entries[0].State != StatePendingApproval {
			t.Fatalf("JoinCatalog() state = %q, want %q", entries[0].State, StatePendingApproval)
		}
	})

	t.Run("a service with no matching entitlement is NotRequested", func(t *testing.T) {
		compute := service("compute", "compute.datumapis.com", "Compute", servicesv1alpha1.PhasePublished)
		services := &servicesv1alpha1.ServiceList{Items: []servicesv1alpha1.Service{*compute}}

		entries := JoinCatalog(services, &servicesv1alpha1.ServiceEntitlementList{})
		if len(entries) != 1 {
			t.Fatalf("JoinCatalog() returned %d entries, want 1", len(entries))
		}
		if entries[0].State != StateNotRequested {
			t.Fatalf("JoinCatalog() state = %q, want %q", entries[0].State, StateNotRequested)
		}
		if entries[0].Entitlement != nil {
			t.Fatalf("JoinCatalog() entitlement = %+v, want nil", entries[0].Entitlement)
		}
	})

	t.Run("result is sorted by display name", func(t *testing.T) {
		zebra := service("zebra", "zebra.datumapis.com", "Zebra", servicesv1alpha1.PhasePublished)
		alpha := service("alpha", "alpha.datumapis.com", "Alpha", servicesv1alpha1.PhasePublished)
		mid := service("mid", "mid.datumapis.com", "Mid", servicesv1alpha1.PhasePublished)

		services := &servicesv1alpha1.ServiceList{Items: []servicesv1alpha1.Service{*zebra, *alpha, *mid}}
		entries := JoinCatalog(services, &servicesv1alpha1.ServiceEntitlementList{})

		if len(entries) != 3 {
			t.Fatalf("JoinCatalog() returned %d entries, want 3", len(entries))
		}
		want := []string{"Alpha", "Mid", "Zebra"}
		for i, w := range want {
			if entries[i].Service.DisplayName != w {
				t.Fatalf("JoinCatalog() order = %v, want %v", displayNames(entries), want)
			}
		}
	})

	t.Run("nil ServiceList yields no entries", func(t *testing.T) {
		if entries := JoinCatalog(nil, nil); len(entries) != 0 {
			t.Fatalf("JoinCatalog(nil, nil) = %+v, want empty", entries)
		}
	})

	t.Run("end-to-end against a single fake clientset serving both scopes", func(t *testing.T) {
		compute := service("compute", "compute.datumapis.com", "Compute", servicesv1alpha1.PhasePublished)
		active := entitlement("compute", servicesv1alpha1.EntitlementPhaseActive, servicesv1alpha1.ReasonEntitlementActive, "ok", ptrNow())
		active.Name = "compute"

		cc, cs := newCatalogFake(withServices(compute), withObjects(active))
		ec := NewClient(cs)

		services, err := cc.ListServices(context.Background())
		if err != nil {
			t.Fatalf("ListServices() = %v", err)
		}
		entitlements, err := ec.List(context.Background())
		if err != nil {
			t.Fatalf("List() = %v", err)
		}

		entries := JoinCatalog(services, entitlements)
		if len(entries) != 1 {
			t.Fatalf("JoinCatalog() returned %d entries, want 1", len(entries))
		}
		if entries[0].State != StateActive {
			t.Fatalf("JoinCatalog() state = %q, want %q", entries[0].State, StateActive)
		}
	})
}

func displayNames(entries []CatalogEntry) []string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Service.DisplayName
	}
	return names
}

// ageAgo is exercised indirectly by the renderer; a direct check guards the
// boundary wording used in status copy.
func TestAgeAgo(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{5 * time.Minute, "5m ago"},
		{2 * time.Hour, "2h ago"},
		{49 * time.Hour, "2d ago"},
	}
	for _, tc := range cases {
		got := ageAgo(metav1.NewTime(time.Now().Add(-tc.d)))
		if got != tc.want {
			t.Errorf("ageAgo(%s) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
