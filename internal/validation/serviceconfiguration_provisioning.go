// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
	"go.miloapis.com/service-catalog/internal/provisioning"
)

// validateProvisioning checks a spec.provisioning declaration against the
// platform allowlist and the intra-document rules that do not need a lookup.
//
// This is the first of the allowlist's two enforcement points. It gives the
// provider a synchronous error instead of a refusal buried in a consumer's
// status later. It is not the binding one: the controller repeats the check
// before every write, because this webhook can be absent from the cluster and a
// configuration admitted under an older allowlist stays in etcd. Enforcing only
// here, or leaving it to RBAC, would describe a control that is not in force:
// the operator writes into consumer control planes as system:masters.
func validateProvisioning(sc *servicesv1alpha1.ServiceConfiguration) field.ErrorList {
	var allErrs field.ErrorList

	if sc.Spec.Provisioning == nil {
		return allErrs
	}

	fldPath := field.NewPath("spec", "provisioning", "resources")
	seen := make(map[string]struct{}, len(sc.Spec.Provisioning.Resources))

	for i, res := range sc.Spec.Provisioning.Resources {
		itemPath := fldPath.Index(i)

		if _, dup := seen[res.Name]; dup {
			allErrs = append(allErrs, field.Duplicate(itemPath.Child("name"), res.Name))
		}
		seen[res.Name] = struct{}{}

		kindPath := itemPath.Child("projection", "kind")
		if _, err := provisioning.Lookup(res.Projection.Kind); err != nil {
			allErrs = append(allErrs, field.Invalid(kindPath, res.Projection.Kind, err.Error()))
		}

		// Kubernetes converts an empty selector to "match everything", which
		// would project a provider's entire source project into every entitled
		// project. Require a selector rather than assume the safe reading.
		selector, err := metav1.LabelSelectorAsSelector(&res.Projection.Selector)
		if err != nil {
			allErrs = append(allErrs, field.Invalid(itemPath.Child("projection", "selector"),
				res.Projection.Selector, err.Error()))
		} else if selector.Empty() {
			allErrs = append(allErrs, field.Required(itemPath.Child("projection", "selector"),
				"must select a subset of the source project's objects; an empty selector would "+
					"project every object in it into every project that enables this service"))
		}
	}

	return allErrs
}

// validateProvisioningSourceProjects enforces that a projection reads only out
// of the project the declaring service is published from.
//
// Without it a provider could name any project as a source and have the
// platform read it, with an identity nothing stops: the operator holds
// system:masters in every project control plane. The Service already records
// its producer project, so ownership is a comparison rather than a new concept.
func validateProvisioningSourceProjects(
	ctx context.Context,
	c client.Reader,
	sc *servicesv1alpha1.ServiceConfiguration,
) field.ErrorList {
	var allErrs field.ErrorList

	if sc.Spec.Provisioning == nil || len(sc.Spec.Provisioning.Resources) == 0 ||
		c == nil || sc.Spec.ServiceRef.Name == "" {
		return allErrs
	}

	// A missing or unreadable Service is reported by
	// validateServiceConfigurationNamePrefixes, which resolves the same
	// reference; staying silent here avoids duplicating that error.
	var svc servicesv1alpha1.Service
	if err := c.Get(ctx, types.NamespacedName{Name: sc.Spec.ServiceRef.Name}, &svc); err != nil {
		return allErrs
	}

	producer := svc.Spec.Owner.ProducerProjectRef.Name
	fldPath := field.NewPath("spec", "provisioning", "resources")

	for i, res := range sc.Spec.Provisioning.Resources {
		if res.Projection.SourceProject == producer {
			continue
		}
		allErrs = append(allErrs, field.Invalid(
			fldPath.Index(i).Child("projection", "sourceProject"),
			res.Projection.SourceProject,
			fmt.Sprintf("must be the producer project of the service this configuration "+
				"describes (%q); a service can only offer resources out of a project it owns", producer),
		))
	}

	return allErrs
}

// validateProvisioningPublishedImmutability constrains edits to a Published
// configuration's provisioning declaration.
//
// Changing a retained entry's kind or source project silently re-points
// everything already installed under that name, so both are frozen. The
// selector stays mutable: adjusting which of a provider's own objects are
// offered is how a provider reaches already-entitled projects without
// republishing. Removing an entry stays permitted; it withdraws the declaration
// and prunes what it installed.
func validateProvisioningPublishedImmutability(
	oldSC, newSC *servicesv1alpha1.ServiceConfiguration,
) field.ErrorList {
	var allErrs field.ErrorList

	if oldSC.Spec.Provisioning == nil || newSC.Spec.Provisioning == nil {
		return allErrs
	}

	old := make(map[string]servicesv1alpha1.ProvisionedResourceSpec, len(oldSC.Spec.Provisioning.Resources))
	for _, res := range oldSC.Spec.Provisioning.Resources {
		old[res.Name] = res
	}

	fldPath := field.NewPath("spec", "provisioning", "resources")
	for i, res := range newSC.Spec.Provisioning.Resources {
		prev, ok := old[res.Name]
		if !ok {
			continue
		}
		itemPath := fldPath.Index(i).Child("projection")
		if prev.Projection.Kind != res.Projection.Kind {
			allErrs = append(allErrs, field.Forbidden(itemPath.Child("kind"),
				"can't be changed once the configuration is published"))
		}
		if prev.Projection.SourceProject != res.Projection.SourceProject {
			allErrs = append(allErrs, field.Forbidden(itemPath.Child("sourceProject"),
				"can't be changed once the configuration is published"))
		}
	}

	return allErrs
}
