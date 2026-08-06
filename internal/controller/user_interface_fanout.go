// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	portalv1alpha1 "go.miloapis.com/milo/pkg/apis/portal/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const portalFieldManagerName = "services-operator-portal"

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
		desiredProvider, err = f.applyProviderPlugin(ctx, sc, svc.Spec.ServiceName, svc.Spec.DisplayName, slug, deprecated)
		if err != nil {
			return err
		}
	}

	if err := f.pruneConsumerPlugin(ctx, sc, desiredConsumer); err != nil {
		return err
	}
	return f.pruneProviderPlugin(ctx, sc, desiredProvider)
}

// Cleanup deletes any ConsumerPortalPlugin/ProviderPortalPlugin owned by sc.
// Used during finalization to release managed state before the owner record
// goes away.
func (f *UserInterfaceFanOut) Cleanup(ctx context.Context, sc *servicesv1alpha1.ServiceConfiguration) error {
	if err := f.pruneConsumerPlugin(ctx, sc, ""); err != nil {
		return err
	}
	return f.pruneProviderPlugin(ctx, sc, "")
}

func toPortalAssets(a servicesv1alpha1.PluginAssets) portalv1alpha1.PluginAssets {
	return portalv1alpha1.PluginAssets{
		BaseURL:      a.BaseURL,
		ManifestPath: a.ManifestPath,
		CABundle:     a.CABundle,
	}
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

	obj := &portalv1alpha1.ConsumerPortalPlugin{
		TypeMeta: metav1.TypeMeta{
			APIVersion: portalv1alpha1.GroupVersion.String(),
			Kind:       "ConsumerPortalPlugin",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: slug,
			Labels: map[string]string{
				labelManagedBy:    labelManagedByValue,
				labelOwnerService: serviceName,
			},
		},
		Spec: portalv1alpha1.ConsumerPortalPluginSpec{
			Slug:        slug,
			DisplayName: displayName,
			Deprecated:  deprecated,
			Suspend:     spec.Suspend,
			Assets:      toPortalAssets(spec.Assets),
			Visibility: portalv1alpha1.PluginVisibility{
				Entitlement: portalv1alpha1.PluginEntitlementRequirement(spec.Visibility.Entitlement),
				FeatureFlag: spec.Visibility.FeatureFlag,
			},
		},
	}
	if err := ctrl.SetControllerReference(sc, obj, f.Scheme); err != nil {
		return "", fmt.Errorf("set controller ref on ConsumerPortalPlugin %q: %w", slug, err)
	}
	//nolint:staticcheck // client.Apply deprecated; milo portal types have no generated apply configurations yet.
	if err := f.Client.Patch(ctx, obj, client.Apply, client.FieldOwner(portalFieldManagerName), client.ForceOwnership); err != nil {
		return "", fmt.Errorf("apply ConsumerPortalPlugin %q: %w", slug, err)
	}
	return slug, nil
}

func (f *UserInterfaceFanOut) applyProviderPlugin(
	ctx context.Context,
	sc *servicesv1alpha1.ServiceConfiguration,
	serviceName, displayName, slug string,
	deprecated bool,
) (string, error) {
	spec := sc.Spec.UserInterface.Provider
	if spec == nil {
		return "", nil
	}

	obj := &portalv1alpha1.ProviderPortalPlugin{
		TypeMeta: metav1.TypeMeta{
			APIVersion: portalv1alpha1.GroupVersion.String(),
			Kind:       "ProviderPortalPlugin",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: slug,
			Labels: map[string]string{
				labelManagedBy:    labelManagedByValue,
				labelOwnerService: serviceName,
			},
		},
		Spec: portalv1alpha1.ProviderPortalPluginSpec{
			Slug:        slug,
			DisplayName: displayName,
			Deprecated:  deprecated,
			Suspend:     spec.Suspend,
			Assets:      toPortalAssets(spec.Assets),
		},
	}
	if err := ctrl.SetControllerReference(sc, obj, f.Scheme); err != nil {
		return "", fmt.Errorf("set controller ref on ProviderPortalPlugin %q: %w", slug, err)
	}
	//nolint:staticcheck // client.Apply deprecated; milo portal types have no generated apply configurations yet.
	if err := f.Client.Patch(ctx, obj, client.Apply, client.FieldOwner(portalFieldManagerName), client.ForceOwnership); err != nil {
		return "", fmt.Errorf("apply ProviderPortalPlugin %q: %w", slug, err)
	}
	return slug, nil
}

func (f *UserInterfaceFanOut) pruneConsumerPlugin(
	ctx context.Context,
	sc *servicesv1alpha1.ServiceConfiguration,
	desiredName string,
) error {
	var list portalv1alpha1.ConsumerPortalPluginList
	if err := f.Client.List(ctx, &list, client.MatchingLabelsSelector{Selector: managedByFanoutSelector}); err != nil {
		if meta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("list ConsumerPortalPlugins: %w", err)
	}
	for i := range list.Items {
		obj := &list.Items[i]
		if !ownedBy(obj.OwnerReferences, sc.UID) {
			continue
		}
		if desiredName != "" && obj.Name == desiredName {
			continue
		}
		if err := f.Client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale ConsumerPortalPlugin %q: %w", obj.Name, err)
		}
	}
	return nil
}

func (f *UserInterfaceFanOut) pruneProviderPlugin(
	ctx context.Context,
	sc *servicesv1alpha1.ServiceConfiguration,
	desiredName string,
) error {
	var list portalv1alpha1.ProviderPortalPluginList
	if err := f.Client.List(ctx, &list, client.MatchingLabelsSelector{Selector: managedByFanoutSelector}); err != nil {
		if meta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("list ProviderPortalPlugins: %w", err)
	}
	for i := range list.Items {
		obj := &list.Items[i]
		if !ownedBy(obj.OwnerReferences, sc.UID) {
			continue
		}
		if desiredName != "" && obj.Name == desiredName {
			continue
		}
		if err := f.Client.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale ProviderPortalPlugin %q: %w", obj.Name, err)
		}
	}
	return nil
}
