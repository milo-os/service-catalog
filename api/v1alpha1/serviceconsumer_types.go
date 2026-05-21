// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConsumerPhase describes the lifecycle state of a ServiceConsumer.
//
// +kubebuilder:validation:Enum=PendingApproval;Active;Denied
type ConsumerPhase string

const (
	// ConsumerPhasePendingApproval indicates the consumer is awaiting
	// provider approval before becoming active.
	ConsumerPhasePendingApproval ConsumerPhase = "PendingApproval"

	// ConsumerPhaseActive indicates the consumer has been approved and the
	// entitlement is active.
	ConsumerPhaseActive ConsumerPhase = "Active"

	// ConsumerPhaseDenied indicates the provider denied the consumer's
	// request for access to the service.
	ConsumerPhaseDenied ConsumerPhase = "Denied"
)

// ApprovalDecision is the provider's decision on a service consumer request.
//
// +kubebuilder:validation:Enum=Approved;Denied
type ApprovalDecision string

const (
	// ApprovalDecisionApproved indicates the provider has approved the
	// consumer's request.
	ApprovalDecisionApproved ApprovalDecision = "Approved"

	// ApprovalDecisionDenied indicates the provider has denied the
	// consumer's request.
	ApprovalDecisionDenied ApprovalDecision = "Denied"
)

// ProviderApproval captures the provider's decision on a GatedByProvider
// service consumer request.
type ProviderApproval struct {
	// Decision is the provider's approval or denial of the consumer request.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=Approved;Denied
	Decision ApprovalDecision `json:"decision"`

	// Message is an optional human-readable explanation of the decision.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=1024
	Message string `json:"message,omitempty"`
}

// ServiceConsumerSpec defines the desired state of a ServiceConsumer.
type ServiceConsumerSpec struct {
	// ServiceRef identifies the Service this consumer record is associated with.
	//
	// +kubebuilder:validation:Required
	ServiceRef ServiceRef `json:"serviceRef"`

	// ConsumerProjectRef identifies the consumer project that requested
	// access to the service.
	//
	// +kubebuilder:validation:Required
	ConsumerProjectRef ConsumerProjectRef `json:"consumerProjectRef"`

	// Approval is the provider's decision for GatedByProvider services.
	// The services controller creates the object; the provider writes only
	// this field.
	//
	// +kubebuilder:validation:Optional
	Approval *ProviderApproval `json:"approval,omitempty"`
}

// ServiceConsumerStatus defines the observed state of a ServiceConsumer.
type ServiceConsumerStatus struct {
	// Phase is the controller-observed lifecycle state of this consumer record.
	//
	// +kubebuilder:validation:Optional
	Phase ConsumerPhase `json:"phase,omitempty"`

	// EntitledAt is the time at which this consumer record became Active.
	//
	// +kubebuilder:validation:Optional
	EntitledAt *metav1.Time `json:"entitledAt,omitempty"`

	// Conditions represent the latest available observations of the
	// consumer record's state.
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

// ServiceConsumer is the Schema for the serviceconsumers API. The services
// controller creates one ServiceConsumer in the provider project's virtual
// control plane for every active or pending ServiceEntitlement. Providers
// never create these directly; they only write the spec.approval field to
// approve or deny GatedByProvider requests.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Project"
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.spec.serviceRef.name`
// +kubebuilder:printcolumn:name="Consumer",type=string,JSONPath=`.spec.consumerProjectRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
type ServiceConsumer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServiceConsumerSpec   `json:"spec,omitempty"`
	Status ServiceConsumerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ServiceConsumerList contains a list of ServiceConsumer.
type ServiceConsumerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ServiceConsumer `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ServiceConsumer{}, &ServiceConsumerList{})
}
