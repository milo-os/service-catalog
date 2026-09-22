package activation

import (
	"fmt"
	"strings"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// ServiceInfo is the identity subset of a Service the activation flow reads. It
// is always derived from a live Service via NewServiceInfo rather than
// hand-authored, so the single-service flow (Gate, Requester, Observe) and the
// catalog-wide flow (JoinCatalog) share one value shape and never drift out of
// sync with the API object.
type ServiceInfo struct {
	// ObjectName is the Service's metadata.name. It is written to
	// spec.serviceRef.name on create (admission rejects the canonical name) and
	// is the object-name fallback when selecting the entitlement.
	ObjectName string

	// CanonicalName is the reverse-DNS service identity (spec.serviceName, e.g.
	// "compute.datumapis.com"). It is the preferred selection key: every
	// reconciled entitlement carries it in status.serviceName, so matching on
	// it avoids mistaking a dependency entitlement for the direct one.
	CanonicalName string

	// DisplayName is the human noun for the service (spec.displayName),
	// capitalized for use at the start of a sentence (e.g. "Compute").
	// Mid-sentence uses are lowercased.
	DisplayName string

	// Description is the plain-English explanation of the service (spec.description).
	Description string

	// EnablementMode reports whether the service can be self-service enabled or
	// requires provider approval (spec.enablementPolicy.mode). A nil
	// EnablementPolicy defaults to SelfService, matching the API's kubebuilder
	// default.
	EnablementMode servicesv1alpha1.EnablementMode
}

// NewServiceInfo maps a live Service onto its identity subset.
func NewServiceInfo(svc *servicesv1alpha1.Service) ServiceInfo {
	mode := servicesv1alpha1.EnablementModeSelfService
	if svc.Spec.EnablementPolicy != nil {
		mode = svc.Spec.EnablementPolicy.Mode
	}
	return ServiceInfo{
		ObjectName:     svc.Name,
		CanonicalName:  svc.Spec.ServiceName,
		DisplayName:    svc.Spec.DisplayName,
		Description:    svc.Spec.Description,
		EnablementMode: mode,
	}
}

// Validate reports whether the required identity fields are set.
func (c ServiceInfo) Validate() error {
	if c.ObjectName == "" {
		return fmt.Errorf("activation: ServiceInfo.ObjectName is required")
	}
	if c.CanonicalName == "" {
		return fmt.Errorf("activation: ServiceInfo.CanonicalName is required")
	}
	if c.DisplayName == "" {
		return fmt.Errorf("activation: ServiceInfo.DisplayName is required")
	}
	return nil
}

// noun returns the display noun for mid-sentence use ("compute").
func (c ServiceInfo) noun() string { return strings.ToLower(c.DisplayName) }

// enableCommand is the copy-pasteable command that submits an enablement
// request, computed from the service's own canonical identity rather than a
// per-caller field so the SDK never drifts out of sync with a hardcoded verb.
func (c ServiceInfo) enableCommand() string { return "datumctl services enable " + c.CanonicalName }

// statusCommand is the copy-pasteable command that checks current status.
func (c ServiceInfo) statusCommand() string { return "datumctl services status " + c.CanonicalName }

// FindService resolves a service by name from a catalog listing, matching the
// canonical name first and falling back to the object name — the same
// preference order SelectEntitlement uses when matching an entitlement to a
// service, so a name that resolves against one also resolves against the other.
//
// Only Published services are considered, matching the filter JoinCatalog
// applies for a catalog listing: that keeps the name spaces identical, so a
// not-found error can always point at `datumctl services list` as the
// authoritative source of valid names.
//
// It is exported because every caller running a Gate needs a ServiceInfo, and
// the only honest way to build one is from a live Service. Without this, each
// adopter re-implements the matching rule this package exists to own.
func FindService(services *servicesv1alpha1.ServiceList, name string) (ServiceInfo, error) {
	if services != nil {
		var fallback *ServiceInfo
		for i := range services.Items {
			svc := &services.Items[i]
			if svc.Spec.Phase != servicesv1alpha1.PhasePublished {
				continue
			}
			info := NewServiceInfo(svc)
			if info.CanonicalName == name {
				return info, nil
			}
			if fallback == nil && info.ObjectName == name {
				fallback = &info
			}
		}
		if fallback != nil {
			return *fallback, nil
		}
	}
	return ServiceInfo{}, fmt.Errorf("service %q not found; run `datumctl services list` and pass the value from the SERVICE NAME column", name)
}
