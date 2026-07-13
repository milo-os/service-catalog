package activation

import (
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// ServiceInfo is the identity subset of a Service the activation flow reads.
// It exists so Config is always derived from a live Service rather than
// hand-authored, and so catalog-wide callers (JoinCatalog) have a value to
// carry alongside a classified state without re-deriving a Config for
// services with no matching entitlement.
type ServiceInfo struct {
	// ObjectName is the Service's metadata.name.
	ObjectName string

	// CanonicalName is the reverse-DNS service identity (spec.serviceName).
	CanonicalName string

	// DisplayName is the human noun for the service (spec.displayName).
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

// ConfigFromService builds the Config the single-service flow (Gate,
// Requester, Observe) needs from a ServiceInfo. This is the only supported way
// to construct a Config — no more hand-authored literals.
func ConfigFromService(info ServiceInfo) Config {
	return Config{
		ObjectName:    info.ObjectName,
		CanonicalName: info.CanonicalName,
		DisplayName:   info.DisplayName,
	}
}
