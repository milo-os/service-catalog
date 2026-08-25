package cmd

import (
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
	"go.miloapis.com/service-catalog/pkg/activation"
)

// resolveService finds the service named by name within services. The matching
// rule lives in the activation package alongside the API it interprets; this
// wrapper keeps the call sites in this package unchanged.
func resolveService(services *servicesv1alpha1.ServiceList, name string) (activation.ServiceInfo, error) {
	return activation.FindService(services, name)
}
