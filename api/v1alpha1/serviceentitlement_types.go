// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EntitlementPhase describes the lifecycle state of a ServiceEntitlement.
//
// +kubebuilder:validation:Enum=PendingApproval;Active;Rejected
type EntitlementPhase string

const (
	// EntitlementPhasePendingApproval indicates the entitlement is awaiting
	// provider approval before becoming active.
	EntitlementPhasePendingApproval EntitlementPhase = "PendingApproval"

	// EntitlementPhaseActive indicates the entitlement is approved and the
	// consumer project has access to the service.
	EntitlementPhaseActive EntitlementPhase = "Active"

	// EntitlementPhaseRejected indicates the entitlement request was denied
	// by the provider.
	EntitlementPhaseRejected EntitlementPhase = "Rejected"
)

// EntitlementOrigin describes how a ServiceEntitlement was created.
//
// +kubebuilder:validation:Enum=Direct;Dependency
type EntitlementOrigin string

const (
	// EntitlementOriginDirect indicates the consumer admin explicitly
	// requested this service entitlement.
	EntitlementOriginDirect EntitlementOrigin = "Direct"

	// EntitlementOriginDependency indicates this entitlement was created
	// automatically to satisfy a dependency of another entitlement.
	EntitlementOriginDependency EntitlementOrigin = "Dependency"
)

// ServiceEntitlementSpec defines the desired state of a ServiceEntitlement.
type ServiceEntitlementSpec struct {
	// ServiceRef identifies the Service the consumer project wants to enable.
	//
	// +kubebuilder:validation:Required
	ServiceRef ServiceRef `json:"serviceRef"`

	// RequestMessage is an optional human-readable message sent to the
	// provider when the service requires GatedByProvider approval.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=1024
	RequestMessage string `json:"requestMessage,omitempty"`
}

// ServiceEntitlementStatus defines the observed state of a ServiceEntitlement.
type ServiceEntitlementStatus struct {
	// Phase is the controller-observed lifecycle state of this entitlement.
	//
	// +kubebuilder:validation:Optional
	Phase EntitlementPhase `json:"phase,omitempty"`

	// Origin indicates whether this entitlement was created directly by a
	// consumer admin or automatically as a dependency of another entitlement.
	//
	// +kubebuilder:validation:Optional
	Origin EntitlementOrigin `json:"origin,omitempty"`

	// DependencyOf is the metadata.name of the ServiceEntitlement that caused
	// this entitlement to be created when origin is Dependency.
	//
	// +kubebuilder:validation:Optional
	DependencyOf string `json:"dependencyOf,omitempty"`

	// EntitledAt is the time at which this entitlement became Active.
	//
	// +kubebuilder:validation:Optional
	EntitledAt *metav1.Time `json:"entitledAt,omitempty"`

	// Conditions represent the latest available observations of the
	// entitlement's state.
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

// ServiceEntitlement is the Schema for the serviceentitlements API. A consumer
// project admin creates one ServiceEntitlement per service they want to use.
// The object is written into the consumer project's virtual control plane and
// the services operator reconciles it into the provider project.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Project"
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.spec.serviceRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Origin",type=string,JSONPath=`.status.origin`
type ServiceEntitlement struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServiceEntitlementSpec   `json:"spec,omitempty"`
	Status ServiceEntitlementStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ServiceEntitlementList contains a list of ServiceEntitlement.
type ServiceEntitlementList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ServiceEntitlement `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ServiceEntitlement{}, &ServiceEntitlementList{})
}
