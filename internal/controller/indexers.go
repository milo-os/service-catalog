// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servicesv1alpha1 "go.miloapis.com/services/api/v1alpha1"
)

const (
	// ServiceServiceNameField indexes Service objects by their
	// canonical reverse-DNS name (spec.serviceName).
	ServiceServiceNameField = ".spec.serviceName"

	// MeterDefinitionMeterNameField indexes MeterDefinition objects by
	// their canonical meter name (spec.meterName).
	MeterDefinitionMeterNameField = ".spec.meterName"

	// MeterDefinitionOwnerServiceField indexes MeterDefinition objects
	// by their owning service (spec.owner.service).
	MeterDefinitionOwnerServiceField = ".spec.owner.service"

	// MonitoredResourceTypeResourceTypeNameField indexes
	// MonitoredResourceType objects by their canonical resource type
	// name (spec.resourceTypeName).
	MonitoredResourceTypeResourceTypeNameField = ".spec.resourceTypeName"

	// MonitoredResourceTypeOwnerServiceField indexes
	// MonitoredResourceType objects by their owning service
	// (spec.owner.service).
	MonitoredResourceTypeOwnerServiceField = ".spec.owner.service"

	// MonitoredResourceTypeGVKField indexes MonitoredResourceType
	// objects by the composite "<group>/<kind>" key drawn from spec.gvk.
	MonitoredResourceTypeGVKField = ".spec.gvk"
)

// AddIndexers registers the field indexes used by the services
// controllers and webhooks to perform cross-resource lookups without
// falling back to full list scans.
func AddIndexers(ctx context.Context, mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		ctx,
		&servicesv1alpha1.Service{},
		ServiceServiceNameField,
		func(obj client.Object) []string {
			svc := obj.(*servicesv1alpha1.Service)
			return []string{svc.Spec.ServiceName}
		},
	); err != nil {
		return err
	}

	if err := mgr.GetFieldIndexer().IndexField(
		ctx,
		&servicesv1alpha1.MeterDefinition{},
		MeterDefinitionMeterNameField,
		func(obj client.Object) []string {
			md := obj.(*servicesv1alpha1.MeterDefinition)
			return []string{md.Spec.MeterName}
		},
	); err != nil {
		return err
	}

	if err := mgr.GetFieldIndexer().IndexField(
		ctx,
		&servicesv1alpha1.MeterDefinition{},
		MeterDefinitionOwnerServiceField,
		func(obj client.Object) []string {
			md := obj.(*servicesv1alpha1.MeterDefinition)
			return []string{md.Spec.Owner.Service}
		},
	); err != nil {
		return err
	}

	if err := mgr.GetFieldIndexer().IndexField(
		ctx,
		&servicesv1alpha1.MonitoredResourceType{},
		MonitoredResourceTypeResourceTypeNameField,
		func(obj client.Object) []string {
			mrt := obj.(*servicesv1alpha1.MonitoredResourceType)
			return []string{mrt.Spec.ResourceTypeName}
		},
	); err != nil {
		return err
	}

	if err := mgr.GetFieldIndexer().IndexField(
		ctx,
		&servicesv1alpha1.MonitoredResourceType{},
		MonitoredResourceTypeOwnerServiceField,
		func(obj client.Object) []string {
			mrt := obj.(*servicesv1alpha1.MonitoredResourceType)
			return []string{mrt.Spec.Owner.Service}
		},
	); err != nil {
		return err
	}

	if err := mgr.GetFieldIndexer().IndexField(
		ctx,
		&servicesv1alpha1.MonitoredResourceType{},
		MonitoredResourceTypeGVKField,
		func(obj client.Object) []string {
			mrt := obj.(*servicesv1alpha1.MonitoredResourceType)
			return []string{GVKIndexKey(mrt.Spec.GVK.Group, mrt.Spec.GVK.Kind)}
		},
	); err != nil {
		return err
	}

	return nil
}

// GVKIndexKey returns the composite key "<group>/<kind>" used to index
// MonitoredResourceType objects by their bound Kubernetes Kind.
func GVKIndexKey(group, kind string) string {
	return group + "/" + kind
}
