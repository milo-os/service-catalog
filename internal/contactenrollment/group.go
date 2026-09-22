// SPDX-License-Identifier: AGPL-3.0-only

package contactenrollment

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	notificationv1alpha1 "go.miloapis.com/milo/pkg/apis/notification/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// EnsureGroup returns the ContactGroup svc.Spec.ContactEnrollment.ContactGroupRef
// names, creating it with the ContactEnrollment defaults if it doesn't exist
// yet. defaultNamespace is used when ContactGroupRef.Namespace is unset.
//
// It does not reconcile an existing group's settings — an operator who wants
// different visibility, description, or provider sync destinations changes
// the group directly (in staff-portal, the same as any other ContactGroup).
// service-catalog only guarantees the group exists; it does not own it
// afterward.
//
// svc.Spec.ContactEnrollment must be non-nil; callers only reach this
// function for a service that has opted in.
func EnsureGroup(ctx context.Context, c client.Client, defaultNamespace string, svc *servicesv1alpha1.Service) (*notificationv1alpha1.ContactGroup, error) {
	ce := svc.Spec.ContactEnrollment
	if ce == nil {
		return nil, fmt.Errorf("service %q has no contactEnrollment configured", svc.Name)
	}

	name := ce.ContactGroupRef.Name
	ns := ce.ContactGroupRef.Namespace
	if ns == "" {
		ns = defaultNamespace
	}

	group := &notificationv1alpha1.ContactGroup{}
	err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, group)
	if err == nil {
		return group, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get ContactGroup %s/%s: %w", ns, name, err)
	}

	group = &notificationv1alpha1.ContactGroup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       newContactGroupSpec(svc, ce),
	}
	if err := c.Create(ctx, group); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Lost a create race; read back what the other writer made
			// rather than assume our own defaults took effect.
			if getErr := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, group); getErr != nil {
				return nil, fmt.Errorf("get ContactGroup %s/%s after losing create race: %w", ns, name, getErr)
			}
			return group, nil
		}
		return nil, fmt.Errorf("create ContactGroup %s/%s: %w", ns, name, err)
	}
	return group, nil
}

// newContactGroupSpec builds the defaults for a newly auto-created
// ContactGroup:
//
//   - DisplayName falls back to the Service's own display name.
//   - Description falls back to a generated one naming the service.
//   - Visibility defaults to public so an opted-out consumer's opt-out is
//     always honored; a group that must enforce membership sets it
//     explicitly to private.
//   - Providers is left empty: ContactGroupSpec.Providers is add-only with
//     immutable IDs (see Milo's ContactGroupSpec XValidation rule), so
//     guessing a provider list ID here would be unrecoverable. An operator
//     attaches the right one afterward, the same way they configure any
//     other auto-created group.
func newContactGroupSpec(svc *servicesv1alpha1.Service, ce *servicesv1alpha1.ContactEnrollment) notificationv1alpha1.ContactGroupSpec {
	displayName := ce.DisplayName
	if displayName == "" {
		displayName = svc.Spec.DisplayName
	}

	description := ce.Description
	if description == "" {
		description = fmt.Sprintf("Consumers registered for %s.", svc.Spec.ServiceName)
	}

	visibility := ce.Visibility
	if visibility == "" {
		visibility = servicesv1alpha1.ContactGroupVisibilityPublic
	}

	return notificationv1alpha1.ContactGroupSpec{
		DisplayName: displayName,
		Description: description,
		Visibility:  notificationv1alpha1.ContactGroupVisibility(visibility),
	}
}
