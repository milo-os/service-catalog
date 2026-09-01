// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const portalFieldManagerName = "services-operator-portal"

// portalGroupVersion is portal.miloapis.com/v1alpha1. ConsumerPortalPlugin
// and ProviderPortalPlugin are each owned and schema-defined by their own
// portal (cloud-portal, staff-portal respectively), not by milo — so unlike
// the billing/quota fan-outs, this one has no generated Go types to import
// and instead builds/reads these objects as unstructured.Unstructured.
var portalGroupVersion = schema.GroupVersion{Group: "portal.miloapis.com", Version: "v1alpha1"}

func portalGVK(kind string) schema.GroupVersionKind {
	return portalGroupVersion.WithKind(kind)
}

// UserInterfaceFanOut materializes the portal plugin(s) declared by a
// ServiceConfiguration's spec.userInterface (ConsumerPortalPlugin,
// ProviderPortalPlugin) via server-side apply, and prunes a previously
// applied plugin whose block has since been removed from the spec.
type UserInterfaceFanOut struct {
	Client client.Client
	Scheme *runtime.Scheme
}

// Reconcile applies the ConsumerPortalPlugin/ProviderPortalPlugin declared
// by sc.Spec.UserInterface and deletes either one, owned by sc, that is no
// longer declared — including both, if spec.userInterface itself has been
// removed entirely. Draft configurations are skipped entirely (Cleanup still
// runs on delete regardless of phase).
func (f *UserInterfaceFanOut) Reconcile(ctx context.Context, sc *servicesv1alpha1.ServiceConfiguration) error {
	if sc.Spec.Phase == servicesv1alpha1.PhaseDraft {
		return nil
	}

	var desiredConsumer, desiredProvider string
	if sc.Spec.UserInterface != nil {
		var svc servicesv1alpha1.Service
		if err := f.Client.Get(ctx, client.ObjectKey{Name: sc.Spec.ServiceRef.Name}, &svc); err != nil {
			return fmt.Errorf("resolve Service %q: %w", sc.Spec.ServiceRef.Name, err)
		}
		slug := encodeName(svc.Spec.ServiceName)
		deprecated := sc.Spec.Phase == servicesv1alpha1.PhaseDeprecated

		var err error
		desiredConsumer, err = f.applyConsumerPlugin(ctx, sc, svc.Spec.ServiceName, svc.Spec.DisplayName, slug, deprecated)
		if err != nil {
			return err
		}
		desiredProvider, err = f.applyProviderPlugin(ctx, sc, svc.Name, svc.Spec.ServiceName, svc.Spec.DisplayName, slug, deprecated)
		if err != nil {
			return err
		}
	}

	if err := f.pruneOne(ctx, sc, "ConsumerPortalPlugin", "ConsumerPortalPluginList", desiredConsumer); err != nil {
		return err
	}
	return f.pruneOne(ctx, sc, "ProviderPortalPlugin", "ProviderPortalPluginList", desiredProvider)
}

// Cleanup deletes any ConsumerPortalPlugin/ProviderPortalPlugin owned by sc.
// Used during finalization to release managed state before the owner record
// goes away.
func (f *UserInterfaceFanOut) Cleanup(ctx context.Context, sc *servicesv1alpha1.ServiceConfiguration) error {
	if err := f.pruneOne(ctx, sc, "ConsumerPortalPlugin", "ConsumerPortalPluginList", ""); err != nil {
		return err
	}
	return f.pruneOne(ctx, sc, "ProviderPortalPlugin", "ProviderPortalPluginList", "")
}

func pluginAssetsMap(a servicesv1alpha1.PluginAssets) map[string]interface{} {
	m := map[string]interface{}{"baseURL": a.BaseURL}
	if a.ManifestPath != "" {
		m["manifestPath"] = a.ManifestPath
	}
	if a.CABundle != "" {
		m["caBundle"] = a.CABundle
	}
	return m
}

// applyConsumerPlugin returns the desired object name (for pruning) when
// sc.Spec.UserInterface.Consumer is set, or "" when it's nil — the caller
// treats an empty desired name as "delete whatever's there."
func (f *UserInterfaceFanOut) applyConsumerPlugin(
	ctx context.Context,
	sc *servicesv1alpha1.ServiceConfiguration,
	serviceName, displayName, slug string,
	deprecated bool,
) (string, error) {
	spec := sc.Spec.UserInterface.Consumer
	if spec == nil {
		return "", nil
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(portalGVK("ConsumerPortalPlugin"))
	obj.SetName(slug)
	obj.SetLabels(map[string]string{
		labelManagedBy:    labelManagedByValue,
		labelOwnerService: serviceName,
	})
	if err := unstructured.SetNestedMap(obj.Object, map[string]interface{}{
		"slug":        slug,
		"displayName": displayName,
		"deprecated":  deprecated,
		"suspend":     spec.Suspend,
		"assets":      pluginAssetsMap(spec.Assets),
		"visibility": map[string]interface{}{
			"entitlement": spec.Visibility.Entitlement,
			"featureFlag": spec.Visibility.FeatureFlag,
		},
	}, "spec"); err != nil {
		return "", fmt.Errorf("build ConsumerPortalPlugin %q spec: %w", slug, err)
	}

	if err := ctrl.SetControllerReference(sc, obj, f.Scheme); err != nil {
		return "", fmt.Errorf("set controller ref on ConsumerPortalPlugin %q: %w", slug, err)
	}
	//nolint:staticcheck // client.Apply deprecated; portal plugins are applied as unstructured with no generated ApplyConfiguration.
	if err := f.Client.Patch(ctx, obj, client.Apply, client.FieldOwner(portalFieldManagerName), client.ForceOwnership); err != nil {
		return "", fmt.Errorf("apply ConsumerPortalPlugin %q: %w", slug, err)
	}
	return slug, nil
}

// applyProviderPlugin returns the desired object name (for pruning) when
// sc.Spec.UserInterface.Provider is set, or "" when it's nil — same
// no-op-on-nil contract as applyConsumerPlugin above.
//
// serviceResourceName vs serviceName: these are two different identifiers
// for the same Service and are easy to transpose — serviceResourceName is
// svc.Name (the Service's own resource/object name, e.g. "compute"), while
// serviceName is svc.Spec.ServiceName (the canonical dotted name, e.g.
// "compute.datumapis.com", used only for the managed-by label here).
// serviceResourceName is what staff-portal's /admin/service-catalog/:name
// route matches against, so it must be svc.Name specifically.
func (f *UserInterfaceFanOut) applyProviderPlugin(
	ctx context.Context,
	sc *servicesv1alpha1.ServiceConfiguration,
	serviceResourceName, serviceName, displayName, slug string,
	deprecated bool,
) (string, error) {
	spec := sc.Spec.UserInterface.Provider
	if spec == nil {
		return "", nil
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(portalGVK("ProviderPortalPlugin"))
	obj.SetName(slug)
	obj.SetLabels(map[string]string{
		labelManagedBy:    labelManagedByValue,
		labelOwnerService: serviceName,
	})
	if err := unstructured.SetNestedMap(obj.Object, map[string]interface{}{
		"slug":        slug,
		"displayName": displayName,
		"deprecated":  deprecated,
		"suspend":     spec.Suspend,
		"assets":      pluginAssetsMap(spec.Assets),
		// Anchors this plugin's portal.page/service extensions to
		// staff-portal's /admin/service-catalog/:name detail page — :name is
		// the Service's resource name (svc.Name in Reconcile, threaded through
		// here as serviceResourceName; see this function's doc comment).
		"serviceRef": map[string]interface{}{
			"name": serviceResourceName,
		},
	}, "spec"); err != nil {
		return "", fmt.Errorf("build ProviderPortalPlugin %q spec: %w", slug, err)
	}

	if err := ctrl.SetControllerReference(sc, obj, f.Scheme); err != nil {
		return "", fmt.Errorf("set controller ref on ProviderPortalPlugin %q: %w", slug, err)
	}
	//nolint:staticcheck // client.Apply deprecated; portal plugins are applied as unstructured with no generated ApplyConfiguration.
	if err := f.Client.Patch(ctx, obj, client.Apply, client.FieldOwner(portalFieldManagerName), client.ForceOwnership); err != nil {
		return "", fmt.Errorf("apply ProviderPortalPlugin %q: %w", slug, err)
	}
	return slug, nil
}

// pruneOne deletes every kind-typed object carrying the fan-out's
// managed-by label, owned by sc, whose name isn't desiredName (an empty
// desiredName deletes anything owned by sc). listKind is the Kind's
// List suffix (e.g. "ConsumerPortalPluginList").
func (f *UserInterfaceFanOut) pruneOne(
	ctx context.Context,
	sc *servicesv1alpha1.ServiceConfiguration,
	kind, listKind, desiredName string,
) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(portalGVK(listKind))
	if err := f.Client.List(ctx, list, client.MatchingLabelsSelector{Selector: managedByFanoutSelector}); err != nil {
		if meta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("list %s: %w", listKind, err)
	}
	for i := range list.Items {
		obj := &list.Items[i]
		if !ownedBy(obj.GetOwnerReferences(), sc.UID) {
			continue
		}
		if desiredName != "" && obj.GetName() == desiredName {
			continue
		}
		if err := f.Client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale %s %q: %w", kind, obj.GetName(), err)
		}
	}
	return nil
}
