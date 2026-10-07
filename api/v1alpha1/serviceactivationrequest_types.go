// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// ServiceActivationAuthorizedAnnotation records that admission authorized
	// this request against the target consumer project. The admission webhook
	// owns this annotation; clients must not set or change it.
	ServiceActivationAuthorizedAnnotation = "services.miloapis.com/activation-authorized"

	// ServiceActivationAuthorizationReasonAnnotation records a stable reason
	// for the admission authorization decision. The admission webhook owns this
	// annotation; clients must not set or change it.
	ServiceActivationAuthorizationReasonAnnotation = "services.miloapis.com/activation-authorization-reason"

	// ServiceActivationAuthorizedValueTrue and
	// ServiceActivationAuthorizedValueFalse are the supported authorization
	// snapshot values.
	ServiceActivationAuthorizedValueTrue  = "true"
	ServiceActivationAuthorizedValueFalse = "false"
)

// ServiceActivationRequestPhase describes the lifecycle state of a
// ServiceActivationRequest.
//
// +kubebuilder:validation:Enum=Pending;Active;Denied;Failed;Disabled
type ServiceActivationRequestPhase string

const (
	// ServiceActivationRequestPhasePending indicates that authorization or
	// entitlement reconciliation has not completed yet.
	ServiceActivationRequestPhasePending ServiceActivationRequestPhase = "Pending"

	// ServiceActivationRequestPhaseActive indicates that the consumer's
	// ServiceEntitlement is active.
	ServiceActivationRequestPhaseActive ServiceActivationRequestPhase = "Active"

	// ServiceActivationRequestPhaseDenied indicates that consumer policy did
	// not authorize the provider actor to activate the service.
	ServiceActivationRequestPhaseDenied ServiceActivationRequestPhase = "Denied"

	// ServiceActivationRequestPhaseFailed indicates that the request could not
	// be fulfilled for a reason other than consumer policy.
	ServiceActivationRequestPhaseFailed ServiceActivationRequestPhase = "Failed"

	// ServiceActivationRequestPhaseDisabled indicates that the consumer
	// disabled the entitlement created for this one-shot request.
	ServiceActivationRequestPhaseDisabled ServiceActivationRequestPhase = "Disabled"
)

// ActorReference identifies the authenticated principal that initiated an
// operation. Admission stamps this value from authentication UserInfo; clients
// must not choose or change it.
type ActorReference struct {
	// Username is the authenticated principal's username.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	Username string `json:"username"`

	// UID is the stable identifier supplied by the authenticator, when one is
	// available.
	//
	// +kubebuilder:validation:Optional
	UID types.UID `json:"uid,omitempty"`
}

// CanonicalServiceReference identifies a Service by its immutable,
// fully-qualified spec.serviceName.
type CanonicalServiceReference struct {
	// Name is the Service's canonical reverse-DNS identifier, for example
	// compute.miloapis.com.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// ServiceActivationRequestSpec defines a provider's one-shot request to
// enable one of its services in a consumer project.
type ServiceActivationRequestSpec struct {
	// ServiceRef identifies the Service to enable by its canonical
	// spec.serviceName. The service must be owned by the provider project in
	// which this request is created.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="serviceRef is immutable"
	ServiceRef CanonicalServiceReference `json:"serviceRef"`

	// ConsumerProjectRef identifies the project in which the service should be
	// enabled.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="consumerProjectRef is immutable"
	ConsumerProjectRef ConsumerProjectRef `json:"consumerProjectRef"`

	// RequestMessage is an optional human-readable explanation for the
	// activation.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=1024
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="requestMessage is immutable"
	RequestMessage string `json:"requestMessage,omitempty"`

	// RequestedBy identifies the authenticated provider actor that submitted
	// the request. The admission webhook always overwrites it from UserInfo.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="requestedBy is immutable"
	RequestedBy ActorReference `json:"requestedBy"`
}

// ServiceEntitlementReference identifies the consumer-side entitlement
// created for a provider activation request.
type ServiceEntitlementReference struct {
	// Name is the entitlement's metadata.name.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// UID is the entitlement's metadata.uid.
	//
	// +kubebuilder:validation:Optional
	UID types.UID `json:"uid,omitempty"`
}

// ServiceActivationRequestStatus defines the observed state of a provider
// activation request.
type ServiceActivationRequestStatus struct {
	// Phase is the controller-observed lifecycle state of this request.
	//
	// +kubebuilder:validation:Optional
	Phase ServiceActivationRequestPhase `json:"phase,omitempty"`

	// EntitlementRef identifies the consumer-side entitlement that fulfills
	// the request.
	//
	// +kubebuilder:validation:Optional
	EntitlementRef *ServiceEntitlementReference `json:"entitlementRef,omitempty"`

	// Conditions represent the latest available observations of the request.
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

// ServiceActivationRequest is a provider-side, one-shot request to enable a
// provider-owned Service in a consumer project. The consumer project must
// authorize the authenticated provider actor through IAM policy.
//
// +genclient
// +genclient:nonNamespaced
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Project"
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.spec.serviceRef.name`
// +kubebuilder:printcolumn:name="Consumer",type=string,JSONPath=`.spec.consumerProjectRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
type ServiceActivationRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServiceActivationRequestSpec   `json:"spec,omitempty"`
	Status ServiceActivationRequestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ServiceActivationRequestList contains a list of ServiceActivationRequest.
type ServiceActivationRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ServiceActivationRequest `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ServiceActivationRequest{}, &ServiceActivationRequestList{})
}
