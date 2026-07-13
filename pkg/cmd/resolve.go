package cmd

import (
	"fmt"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
	"go.miloapis.com/service-catalog/pkg/activation"
)

// resolveService finds the service named by name within services, matching
// the canonical name first and falling back to the object name — the same
// preference order activation.SelectEntitlement uses when matching an
// entitlement to a service, kept consistent so a name that resolves against
// one also resolves against the other.
//
// Only Published services are considered, matching the filter
// activation.JoinCatalog applies for `services list`: that keeps the two
// commands' name spaces identical, so a not-found error here can always point
// at `services list` as the authoritative source of valid names.
func resolveService(services *servicesv1alpha1.ServiceList, name string) (activation.ServiceInfo, error) {
	if services != nil {
		var fallback *activation.ServiceInfo
		for i := range services.Items {
			svc := &services.Items[i]
			if svc.Spec.Phase != servicesv1alpha1.PhasePublished {
				continue
			}
			info := activation.NewServiceInfo(svc)
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
	return activation.ServiceInfo{}, fmt.Errorf("service %q not found; run `datumctl services list` to see available services", name)
}
