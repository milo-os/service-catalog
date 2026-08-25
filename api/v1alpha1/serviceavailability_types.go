// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LocationClassName is the name of a LocationClass a Location can be backed
// by. It is the name of a real object, chosen by whoever owns the capacity
// behind that class, so this API cannot enumerate the set in advance.
//
// The names below are the classes the platform has published so far and are
// kept as constants for callers that reference them directly. Naming a class
// that does not exist is allowed: nothing is projected for it until a Location
// of that class shows up.
//
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=253
type LocationClassName string

const (
	// LocationClassDatumManaged is a shared PoP operated by the Datum
	// platform team.
	LocationClassDatumManaged LocationClassName = "datum-managed"

	// LocationClassProviderDedicated is infrastructure a service provider
	// dedicates to a specific consumer.
	LocationClassProviderDedicated LocationClassName = "provider-dedicated"

	// LocationClassSelfManaged is consumer-registered infrastructure not
	// operated by the platform.
	LocationClassSelfManaged LocationClassName = "self-managed"
)

// ServiceLocationConfig declares which location classes a service version
// supports. It uses class selectors rather than specific location names so
// that new PoPs of a supported class become available to entitled projects
// without requiring a new ServiceConfiguration version.
type ServiceLocationConfig struct {
	// SupportedClasses is the set of location class names this service version
	// runs on. A Location is projected into an entitled project only when the
	// name of the LocationClass backing it appears here.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	// +listType=set
	SupportedClasses []LocationClassName `json:"supportedClasses"`
}

// LocationRef is a reference to a platform Location resource (cluster-scoped).
// Location lives in a separate API group; this reference only constrains the
// shape so ServiceAvailability does not take a compile-time dependency on the
// Location Go type. The name is resolved against locations.miloapis.com first
// and networking.datumapis.com second.
type LocationRef struct {
	// Name is the metadata.name of the referenced Location.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// ServiceAvailabilitySpec defines the desired state of a
// ServiceAvailability.
//
// A ServiceAvailability records that a specific service is deployed and
// validated at a specific Location. It decouples "the PoP exists and
// hardware is Ready" (a Location concern) from "this service is
// operational here" (a service-operator concern). Each service operator
// creates a ServiceAvailability when it completes deployment and health
// checks at a Location; the LocationBindingReconciler reads it as the
// third gate when deciding which locations to project into entitled
// projects.
//
// Both serviceRef and locationRef are immutable: a ServiceAvailability
// records availability for one (service, location) pair for its entire
// lifetime. Re-pointing it would silently rewrite that meaning, so a new
// pairing ships as a new object instead.
type ServiceAvailabilitySpec struct {
	// ServiceRef identifies the Service this availability record applies
	// to by metadata.name. Immutable.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="serviceRef is immutable"
	ServiceRef ServiceRef `json:"serviceRef"`

	// LocationRef identifies the platform Location this availability record
	// applies to. Immutable.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="locationRef is immutable"
	LocationRef LocationRef `json:"locationRef"`
}

// ServiceAvailabilityStatus defines the observed state of a
// ServiceAvailability. The reconciler owns the Available condition: it is
// True when the service operator reports the service deployed and validated
// at the location.
type ServiceAvailabilityStatus struct {
	// Conditions represent the latest available observations of the
	// availability's state. The Available condition is the gate the
	// LocationBindingReconciler reads.
	//
	// +kubebuilder:validation:Optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ObservedGeneration is the most recent generation observed by the
	// controller.
	//
	// +kubebuilder:validation:Optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// ServiceAvailability is the Schema for the serviceavailabilities API. It
// is the third gate in the location three-gate model: it asserts that a
// service is deployed and operational at a specific Location, decoupled
// from the Location's own hardware readiness.
//
// +genclient
// +genclient:nonNamespaced
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.spec.serviceRef.name`
// +kubebuilder:printcolumn:name="Location",type=string,JSONPath=`.spec.locationRef.name`
// +kubebuilder:printcolumn:name="Available",type=string,JSONPath=`.status.conditions[?(@.type=="Available")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Platform"
type ServiceAvailability struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServiceAvailabilitySpec   `json:"spec,omitempty"`
	Status ServiceAvailabilityStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ServiceAvailabilityList contains a list of ServiceAvailability.
type ServiceAvailabilityList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ServiceAvailability `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ServiceAvailability{}, &ServiceAvailabilityList{})
}
