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

// Condition types and reasons written to a ServiceEntitlement's status. They are
// exported so clients (CLIs, SDKs) branch on named constants instead of
// string-matching the controller's output.
const (
	// ConditionTypeReady is the single condition type written on a
	// ServiceEntitlement. It is True only while the entitlement is Active.
	ConditionTypeReady = "Ready"

	// ReasonEntitlementActive is the Ready reason when the entitlement is active
	// and usable.
	ReasonEntitlementActive = "EntitlementActive"

	// ReasonEntitlementPendingApproval is the Ready reason while the request
	// awaits a provider approval decision.
	ReasonEntitlementPendingApproval = "EntitlementPendingApproval"

	// ReasonEntitlementRejected is the Ready reason when the provider denied the
	// request.
	ReasonEntitlementRejected = "EntitlementRejected"

	// ReasonServiceNotPublished is the Ready reason when the referenced service
	// is missing or not yet published. Clients treat it as "unavailable" rather
	// than a denial.
	ReasonServiceNotPublished = "ServiceNotPublished"

	// ConditionTypeSuspended is written on both ServiceEntitlement and
	// ServiceConsumer. It mirrors resourcemanager.miloapis.com Project's own
	// Suspended condition and is orthogonal to status.phase: a PendingApproval
	// or Active entitlement/consumer can independently be Suspended=True while
	// its owning project is suspended, then resume without re-running approval
	// once the project is reinstated.
	ConditionTypeSuspended = "Suspended"

	// ReasonProjectSuspended is the Suspended=True reason while the owning
	// consumer project is suspended.
	ReasonProjectSuspended = "ProjectSuspended"

	// ReasonProjectActive is the Suspended=False reason once the owning
	// consumer project is no longer suspended.
	ReasonProjectActive = "ProjectActive"

	// ConditionTypePaused is written on ServiceConsumer by the provider that
	// owns it, to confirm — separately from ConditionTypeSuspended — whether
	// that provider's Suspend/Resume hooks have finished running for the
	// current suspension signal. It is intentionally a distinct condition:
	// ConditionTypeSuspended is the platform's inbound signal, while
	// ConditionTypePaused is the provider's outbound confirmation the
	// platform rolls up across services to gate the project's
	// Suspending -> Suspended (and Reinstating -> Active) transition.
	// Collapsing the two onto one condition would leave the platform unable
	// to tell "told to pause" from "confirmed paused".
	ConditionTypePaused = "Paused"

	// ReasonPaused is the Paused=True reason once the provider's Suspend
	// hooks have run for the current suspension signal.
	ReasonPaused = "Paused"

	// ReasonActive is the Paused=False reason once the provider's Resume
	// hooks have run after the suspension signal cleared.
	ReasonActive = "Active"

	// ConditionTypeProvisioned reports whether the resources the service
	// declared for a consumer project actually arrived. It is deliberately
	// separate from ConditionTypeReady: Ready means access was granted and any
	// approval passed, Provisioned means delivery happened. Collapsing them
	// would make a transient apply failure indistinguishable from a provider
	// denial, which is the misattribution this reporting exists to prevent.
	ConditionTypeProvisioned = "Provisioned"

	// ReasonProvisioned is the Provisioned=True reason when every declared
	// resource was installed.
	ReasonProvisioned = "Provisioned"

	// ReasonNothingToProvision is the Provisioned=True reason when the service
	// declares no resources. Nothing was owed, so nothing is outstanding.
	ReasonNothingToProvision = "NothingToProvision"

	// ReasonPartiallyProvisioned is the Provisioned=False reason when some
	// declared resources were installed and others could not be.
	ReasonPartiallyProvisioned = "PartiallyProvisioned"

	// ReasonNotProvisioned is the Provisioned=False reason when no declared
	// resource could be installed.
	ReasonNotProvisioned = "NotProvisioned"

	// ReasonEntitlementNotActive is the Provisioned=False reason while the
	// entitlement is not Active. Provisioning follows approval; it does not
	// anticipate it.
	ReasonEntitlementNotActive = "EntitlementNotActive"
)

// ProvisionedResourceState is the delivery state of one declared resource.
//
// +kubebuilder:validation:Enum=Installed;Failed;Unprovisionable
type ProvisionedResourceState string

const (
	// ProvisionedResourceStateInstalled indicates every object the declaration
	// resolved to is present in the consumer project.
	ProvisionedResourceStateInstalled ProvisionedResourceState = "Installed"

	// ProvisionedResourceStateFailed indicates delivery was attempted and
	// failed for a reason that may be transient; it is retried.
	ProvisionedResourceStateFailed ProvisionedResourceState = "Failed"

	// ProvisionedResourceStateUnprovisionable indicates delivery cannot succeed
	// as declared and retrying will not help — the kind is not served by this
	// project's control plane, or the platform allowlist does not admit it.
	// This is reported rather than skipped: "your plane does not serve the kind
	// this service says you need" is precisely what a consumer must be told.
	ProvisionedResourceStateUnprovisionable ProvisionedResourceState = "Unprovisionable"
)

// ProvisionedResourceStatus is the per-resource ledger entry for one
// declaration, in the consumer's own control plane.
type ProvisionedResourceStatus struct {
	// Name is the declaration's spec.provisioning.resources[].name.
	//
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Kind is the group and kind that was installed.
	//
	// +kubebuilder:validation:Optional
	Kind *GVKRef `json:"kind,omitempty"`

	// State is the delivery outcome for this declaration.
	//
	// +kubebuilder:validation:Required
	State ProvisionedResourceState `json:"state"`

	// ObjectCount is how many objects this declaration resolved to and
	// installed.
	//
	// +kubebuilder:validation:Optional
	ObjectCount int32 `json:"objectCount,omitempty"`

	// Reason is a machine-readable cause, set when State is not Installed.
	//
	// +kubebuilder:validation:Optional
	Reason string `json:"reason,omitempty"`

	// Message names the service, the resource, and what a consumer can act on
	// or escalate with. A generic apply error is not an acceptable value here.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=1024
	Message string `json:"message,omitempty"`

	// AuthorizationEstablished reports whether the platform established the
	// consumer project's own authorization to use the referenced source
	// objects, where the target API performs its own permission check.
	//
	// False means the objects exist and work on the strength of the installing
	// identity rather than the consumer's. Nothing in this version of
	// provisioning establishes such a grant, so it is false for every kind
	// whose target API checks permissions. See the allowlist.
	//
	// +kubebuilder:validation:Optional
	AuthorizationEstablished bool `json:"authorizationEstablished,omitempty"`
}

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

	// ServiceName is the canonical service identifier resolved from
	// spec.serviceRef, e.g. "compute.datumapis.com". Set by the controller
	// on first successful reconcile so consumers can always read the
	// canonical name regardless of what was written to spec.serviceRef.name.
	//
	// +kubebuilder:validation:Optional
	ServiceName string `json:"serviceName,omitempty"`

	// ObservedGeneration is the most recent generation observed by the
	// controller.
	//
	// +kubebuilder:validation:Optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ProvisionedResources is the per-resource delivery ledger for the
	// service's spec.provisioning declaration.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	ProvisionedResources []ProvisionedResourceStatus `json:"provisionedResources,omitempty"`

	// LastProvisioningEvaluation is when provisioning was last evaluated for
	// this entitlement, successfully or not.
	//
	// Recorded so that a fan-out which has silently stopped running is visible
	// on the object itself. The failure mode it exists for has already been
	// observed: location projection stopped in staging and surfaced weeks later
	// as an unrelated component reporting a downstream symptom, because a
	// projection that stops reconciling is otherwise indistinguishable from a
	// project that legitimately has nothing.
	//
	// +kubebuilder:validation:Optional
	LastProvisioningEvaluation *metav1.Time `json:"lastProvisioningEvaluation,omitempty"`
}

// ServiceEntitlement is the Schema for the serviceentitlements API. A consumer
// project admin creates one ServiceEntitlement per service they want to use.
// The object is written into the consumer project's virtual control plane and
// the services operator reconciles it into the provider project.
//
// +genclient
// +genclient:nonNamespaced
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Project"
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.spec.serviceRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Origin",type=string,JSONPath=`.status.origin`
// +kubebuilder:printcolumn:name="Suspended",type=string,JSONPath=`.status.conditions[?(@.type=="Suspended")].status`
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
