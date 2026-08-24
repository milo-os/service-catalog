// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ServiceConfigurationSpec defines the desired state of a
// ServiceConfiguration.
//
// A ServiceConfiguration is the single provider-facing document that
// describes everything a service contributes to Milo beyond its identity
// record: its monitored resource types (the Kubernetes Kinds billing and
// dashboards know about) and its meters (the billable dimensions those
// Kinds emit). The services operator fans this document out into the
// downstream CRDs consumed by billing; providers never author those
// directly.
//
// Canonical names on meters and monitored resource types must still be
// prefixed by the referenced service's spec.serviceName. The webhook
// resolves spec.serviceRef and enforces the prefix; the API type only
// constrains the shape.
//
// spec.phase is the provider-declared lifecycle intent:
// Draft -> Published -> Deprecated -> Retired. Draft documents are not
// fanned out. The controller mirrors that intent via conditions; it does
// not transition the phase itself.
type ServiceConfigurationSpec struct {
	// ServiceRef points at the Service this document configures. The
	// reference is by metadata.name of the cluster-scoped Service
	// resource; the webhook resolves it to the Service's canonical
	// spec.serviceName for prefix enforcement.
	//
	// +kubebuilder:validation:Required
	ServiceRef ServiceReference `json:"serviceRef"`

	// Phase is the provider-declared lifecycle state of this
	// configuration. Allowed transitions are forward-only:
	// Draft -> Published -> Deprecated -> Retired.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=Draft;Published;Deprecated;Retired
	// +kubebuilder:default=Draft
	Phase Phase `json:"phase"`

	// Version is an optional human-readable version string for this
	// configuration document (e.g. "v1", "2024-01-15"). It has no
	// semantic meaning to the controller and is surfaced as a table
	// column for operator convenience.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=64
	Version string `json:"version,omitempty"`

	// MonitoredResourceTypes declares the Kubernetes Kinds this service
	// emits usage for, together with the closed set of labels each
	// Kind's usage events may carry. Entries are keyed by .type, which
	// must be unique within the document.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=128
	// +listType=map
	// +listMapKey=type
	MonitoredResourceTypes []MonitoredResourceTypeSpec `json:"monitoredResourceTypes,omitempty"`

	// Metrics declares metric descriptors for this service. Each entry becomes
	// a MeterDefinition in the billing system when routed via spec.billing.
	// Replaces spec.meters[].
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=256
	// +listType=map
	// +listMapKey=name
	Metrics []MetricSpec `json:"metrics,omitempty"`

	// Charges declares Usage, OneTime, and Recurring charges for this
	// service. Fans out to ServicePricing resources distinguished by
	// chargeType. Usage charges reference metrics by name; metrics
	// themselves stay telemetry/quota-only.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=64
	// +listType=map
	// +listMapKey=name
	Charges []ServiceChargeSpec `json:"charges,omitempty"`

	// Billing declares routing from metrics to monitored resource types.
	// Fans out into MeterDefinition billing CRDs.
	//
	// +kubebuilder:validation:Optional
	Billing *ServiceBillingConfig `json:"billing,omitempty"`

	// Quota declares quota limits and metric rules for this service.
	// Fans out into ResourceRegistration and ClaimCreationPolicy quota CRDs.
	//
	// +kubebuilder:validation:Optional
	Quota *ServiceQuotaConfig `json:"quota,omitempty"`

	// UserInterface declares this service's portal plugin(s). Fans out into
	// ConsumerPortalPlugin (cloud-portal) and/or ProviderPortalPlugin
	// (staff-portal) portal.miloapis.com CRDs. A service may publish to
	// either portal, both, or neither.
	//
	// +kubebuilder:validation:Optional
	UserInterface *UserInterfaceSpec `json:"userInterface,omitempty"`

	// Locations declares which location classes this service version
	// supports. The LocationBindingReconciler uses it together with
	// Location readiness and ServiceAvailability to decide which locations
	// to project into entitled projects. Class selectors are used rather
	// than specific location names so new PoPs of a supported class become
	// available without a new configuration version.
	//
	// +kubebuilder:validation:Optional
	Locations *ServiceLocationConfig `json:"locations,omitempty"`

	// Provisioning declares resources the platform installs into every project
	// holding an Active ServiceEntitlement for this service, and removes when
	// that entitlement stops being Active.
	//
	// +kubebuilder:validation:Optional
	Provisioning *ServiceProvisioningConfig `json:"provisioning,omitempty"`

	// DefaultOffer is the Offer name applied to new BillingAccounts.
	// Typically set only on the billing.miloapis.com ServiceConfiguration.
	// When set, the referenced Offer must exist, have launchStage GA, and
	// carry a non-empty servicePricings snapshot.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=253
	DefaultOffer string `json:"defaultOffer,omitempty"`

	// MigrateFromOffer is a one-shot. When set with defaultOffer, the
	// billing-entitlement-defaults controller patches every
	// BillingEntitlement whose offerRef equals this name to defaultOffer,
	// then clears this field. Entitlements pointing at any other Offer
	// are left unchanged. The named Offer does not need to still exist.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=253
	MigrateFromOffer string `json:"migrateFromOffer,omitempty"`
}

// ServiceReference identifies the Service a ServiceConfiguration applies
// to by metadata.name. The webhook resolves the reference to the
// Service's canonical spec.serviceName for name-prefix enforcement.
type ServiceReference struct {
	// Name is the metadata.name of the cluster-scoped Service resource
	// this ServiceConfiguration configures.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// MonitoredResourceTypeSpec is a monitored resource type declared by
// a ServiceConfiguration. The fan-out produces one
// billing.miloapis.com/MonitoredResourceType per entry.
type MonitoredResourceTypeSpec struct {
	// Type is the canonical, user-facing identifier for this resource
	// type (e.g. "compute.miloapis.com/Instance"). Must be prefixed by
	// the referenced Service's spec.serviceName and unique within
	// spec.monitoredResourceTypes. Immutable once the
	// ServiceConfiguration is Published.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Type string `json:"type"`

	// DisplayName is a human-readable name surfaced in portals and
	// dashboards alongside the canonical type.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName,omitempty"`

	// Description is a plain-English explanation of what the resource
	// type represents.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=1024
	Description string `json:"description,omitempty"`

	// GVK pins the resource type to a Kubernetes Kind. Version is
	// deliberately omitted: billability is a property of the Kind, not
	// of a specific API version. Immutable once the
	// ServiceConfiguration is Published.
	//
	// +kubebuilder:validation:Required
	GVK GVKRef `json:"gvk"`

	// Labels is the closed set of descriptive labels that usage events
	// against this resource type are permitted to carry. Events whose
	// labels are not in this set are rejected at the edge.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=64
	// +listType=map
	// +listMapKey=name
	Labels []MonitoredResourceLabel `json:"labels,omitempty"`
}

// GVKRef identifies a Kubernetes Kind by group and kind. Version is
// intentionally excluded so API version evolution does not require a
// new monitored resource type entry.
type GVKRef struct {
	// Group is the Kubernetes API group of the Kind (e.g.
	// "compute.miloapis.com").
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Group string `json:"group"`

	// Kind is the Kubernetes Kind (e.g. "Instance").
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Kind string `json:"kind"`
}

// MonitoredResourceLabel declares a single descriptive label that
// usage events against the resource type may carry.
type MonitoredResourceLabel struct {
	// Name is the label key as it will appear on usage events (e.g.
	// "region", "zone", "tier"). It is the map key for the enclosing
	// list.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Description is a plain-English explanation of what the label
	// conveys.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=512
	Description string `json:"description,omitempty"`
}

// MetricKind mirrors google.api.MetricDescriptor.MetricKind.
//
// +kubebuilder:validation:Enum=Delta;Gauge;Cumulative
type MetricKind string

const (
	MetricKindDelta      MetricKind = "Delta"
	MetricKindGauge      MetricKind = "Gauge"
	MetricKindCumulative MetricKind = "Cumulative"
)

// MetricSpec is a single metric descriptor declared by a ServiceConfiguration.
type MetricSpec struct {
	// Name is the canonical metric identifier prefixed by the service name,
	// e.g. "compute.datumapis.com/instance/cpu-seconds". Immutable once Published.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// DisplayName is a human-readable label shown in portals and invoices.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName,omitempty"`

	// Description is a plain-English explanation of what the metric measures.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=1024
	Description string `json:"description,omitempty"`

	// Kind is the metric kind. Immutable once Published.
	//
	// +kubebuilder:validation:Required
	Kind MetricKind `json:"kind"`

	// Unit is the UCUM emission unit, e.g. "s", "By", "{request}", "1".
	// This is the unit the producer emits. Immutable once Published.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Unit string `json:"unit"`

	// Dimensions is an ordered list of attribute keys that downstream
	// systems may group, filter, or price by (e.g. "model", "region",
	// "tier"). Each key must be declared here for a producer to attach it
	// to a usage event; the billing validator quarantines events carrying
	// undeclared dimension keys. Adding a dimension is additive; removing
	// one is a breaking change and must ship as a new meter. Fans out to
	// MeterDefinition.spec.measurement.dimensions.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	Dimensions []string `json:"dimensions,omitempty"`
}

// ServiceBillingConfig groups all billing routing declarations.
type ServiceBillingConfig struct {
	// ConsumerDestinations routes metrics to monitored resource types for billing.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=64
	// +listType=map
	// +listMapKey=monitoredResourceType
	ConsumerDestinations []BillingConsumerDestination `json:"consumerDestinations,omitempty"`

	// QuotaGating controls whether quota for this service is gated on the
	// account's active BillingEntitlement Offer.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=OrganizationDefault
	QuotaGating QuotaGatingMode `json:"quotaGating,omitempty"`
}

// BillingConsumerDestination routes a set of metrics to a single monitored
// resource type for billing attribution.
type BillingConsumerDestination struct {
	// MonitoredResourceType is the canonical type identifier, e.g.
	// "compute.datumapis.com/Instance". Must match a spec.monitoredResourceTypes[].type.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	MonitoredResourceType string `json:"monitoredResourceType"`

	// Metrics lists the metric names routed to this resource type for billing.
	// Each entry must match a spec.metrics[].name.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=256
	// +listType=set
	Metrics []string `json:"metrics"`
}

// ServiceQuotaConfig groups all quota declarations.
type ServiceQuotaConfig struct {
	// Limits declares per-consumer quota ceilings. Each entry fans out to a
	// ResourceRegistration in the quota system.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=128
	// +listType=map
	// +listMapKey=name
	Limits []QuotaLimitSpec `json:"limits,omitempty"`

	// MetricRules declares ClaimCreationPolicy CRDs that gate resource creation
	// by quota availability. The selector uses apiGroup + kind only; the fan-out
	// resolves the preferred API version at reconcile time.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=128
	MetricRules []QuotaMetricRule `json:"metricRules,omitempty"`
}

// QuotaLimitSpec declares a single quota ceiling for a metric.
type QuotaLimitSpec struct {
	// Name is a unique identifier for this limit within the ServiceConfiguration.
	// Immutable once Published.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// DisplayName is a human-readable name for this quota surfaced in portals
	// and dashboards (e.g. "Instances"). Propagated to the generated
	// ResourceRegistration's kubernetes.io/display-name annotation.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName,omitempty"`

	// Description is a plain-English explanation of what this quota limits
	// (e.g. "Maximum number of compute instances per project"). Propagated to
	// the generated ResourceRegistration's kubernetes.io/description annotation.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=1024
	Description string `json:"description,omitempty"`

	// Metric is the metric name this limit applies to.
	// Must match a spec.metrics[].name. Immutable once Published.
	//
	// +kubebuilder:validation:Required
	Metric string `json:"metric"`

	// ConsumerType identifies the resource kind that receives quota grants.
	// Immutable once Published.
	//
	// +kubebuilder:validation:Required
	ConsumerType QuotaConsumerType `json:"consumerType"`

	// Unit is the quota unit expression, e.g. "1/{project}". Immutable once Published.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Unit string `json:"unit"`

	// DefaultLimit is the quota granted to new consumers on service activation.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=0
	DefaultLimit int64 `json:"defaultLimit"`

	// MaxLimit is the maximum quota any override may grant. Zero means no cap.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Minimum=0
	MaxLimit int64 `json:"maxLimit,omitempty"`
}

// QuotaConsumerType identifies the Kubernetes resource kind that receives quota.
type QuotaConsumerType struct {
	// +kubebuilder:validation:Required
	APIGroup string `json:"apiGroup"`

	// +kubebuilder:validation:Required
	Kind string `json:"kind"`
}

// QuotaMetricRule declares a ClaimCreationPolicy: which resource kind triggers
// quota claim creation, and what metric costs are incurred per creation.
type QuotaMetricRule struct {
	// Selector identifies the resource kind by apiGroup + kind. Version is
	// intentionally omitted; the fan-out resolves it via the discovery API so
	// this config does not need updating when API versions change.
	//
	// +kubebuilder:validation:Required
	Selector QuotaMetricRuleSelector `json:"selector"`

	// MetricCosts maps metric names to integer amounts claimed per resource
	// creation. Each key must match a spec.metrics[].name.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinProperties=1
	MetricCosts map[string]int64 `json:"metricCosts"`
}

// QuotaMetricRuleSelector identifies a resource kind without pinning a version.
type QuotaMetricRuleSelector struct {
	// APIGroup is the Kubernetes API group, e.g. "compute.datumapis.com".
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	APIGroup string `json:"apiGroup"`

	// Kind is the Kubernetes Kind, e.g. "Workload".
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Kind string `json:"kind"`
}

// UserInterfaceSpec declares a service's portal plugin(s). At least one of
// Consumer/Provider is expected to be set for the fan-out to do anything;
// both may be set to publish to both portals from one ServiceConfiguration.
type UserInterfaceSpec struct {
	// Consumer declares a plugin for cloud-portal, the customer-facing
	// portal. Fans out to a ConsumerPortalPlugin.
	//
	// +kubebuilder:validation:Optional
	Consumer *ConsumerUserInterfaceSpec `json:"consumer,omitempty"`

	// Provider declares a plugin for staff-portal, the internal operator
	// portal. Fans out to a ProviderPortalPlugin.
	//
	// +kubebuilder:validation:Optional
	Provider *ProviderUserInterfaceSpec `json:"provider,omitempty"`
}

// PluginAssets locates a plugin's built Module Federation bundle. Mirrors
// portal.miloapis.com's PluginAssets shape (kept as a separate local type,
// not imported from milo, so this CRD's schema stays self-contained — the
// same convention QuotaConsumerType already follows relative to
// quota.miloapis.com's ConsumerType); the fan-out controller maps between
// the two when building the downstream CRD.
type PluginAssets struct {
	// BaseURL is the HTTPS origin, operated by this service, serving the
	// plugin's built assets (remoteEntry.js, chunks, and the manifest at
	// ManifestPath).
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https://`
	BaseURL string `json:"baseURL"`

	// ManifestPath is the path to plugin-manifest.json under BaseURL.
	// Defaults to "/plugin-manifest.json".
	//
	// +kubebuilder:validation:Optional
	ManifestPath string `json:"manifestPath,omitempty"`

	// CABundle is an optional PEM-encoded CA certificate bundle for an
	// internal CA fronting BaseURL.
	//
	// +kubebuilder:validation:Optional
	CABundle string `json:"caBundle,omitempty"`
}

// PluginVisibility gates whether cloud-portal shows a plugin's extensions
// for a given project. No provider equivalent — staff-portal has no
// per-project entitlement concept.
type PluginVisibility struct {
	// Entitlement controls project-level gating: "Required" means a project
	// must have an Active ServiceEntitlement for this service to see the
	// plugin; "None" means every project sees it.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=Required;None
	Entitlement string `json:"entitlement"`

	// FeatureFlag, when set, additionally gates visibility on an OpenFeature
	// flag key evaluated by cloud-portal.
	//
	// +kubebuilder:validation:Optional
	FeatureFlag string `json:"featureFlag,omitempty"`
}

// ConsumerUserInterfaceSpec declares a cloud-portal plugin. Slug,
// DisplayName, and Deprecated are not declared here — the fan-out derives
// them from the resolved Service and this ServiceConfiguration's own
// spec.phase, the same way other fan-outs resolve serviceName rather than
// duplicating it per-block.
type ConsumerUserInterfaceSpec struct {
	// Suspend is a platform-operator kill switch. A suspended plugin is
	// never served, regardless of manifest health.
	//
	// +kubebuilder:validation:Optional
	Suspend bool `json:"suspend,omitempty"`

	// Assets locates the plugin's built Module Federation bundle.
	//
	// +kubebuilder:validation:Required
	Assets PluginAssets `json:"assets"`

	// Visibility gates whether a project sees this plugin's extensions.
	//
	// +kubebuilder:validation:Required
	Visibility PluginVisibility `json:"visibility"`
}

// ProviderUserInterfaceSpec declares a staff-portal plugin.
type ProviderUserInterfaceSpec struct {
	// Suspend is a platform-operator kill switch. A suspended plugin is
	// never served, regardless of manifest health.
	//
	// +kubebuilder:validation:Optional
	Suspend bool `json:"suspend,omitempty"`

	// Assets locates the plugin's built Module Federation bundle.
	//
	// +kubebuilder:validation:Required
	Assets PluginAssets `json:"assets"`
}

// ServiceProvisioningConfig declares the resources a service needs installed in
// a consumer project before that project can use it. It is the authoritative
// statement of what the service manages there.
//
// A provider embeds the objects it wants installed, verbatim. Nothing is
// substituted: every entitled project receives the same object under the same
// name. The bound is therefore not on the content but on the destination and
// the shape. Objects reach only projects that created an entitlement, only once
// that entitlement is Active, and only in a form the platform can own, label,
// and reclaim. That matters because the operator writes into consumer control
// planes as system:masters, where RBAC is no ceiling.
type ServiceProvisioningConfig struct {
	// Resources declares what to install. A declaration fans out across every
	// entitled project, so the cap bounds the blast radius of one configuration
	// and belongs to the security model, not to operations.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	Resources []ProvisionedResourceSpec `json:"resources,omitempty"`
}

// ProvisionedResourceSpec is a single declaration of what to install into
// entitled consumer projects.
type ProvisionedResourceSpec struct {
	// Name identifies this declaration within the ServiceConfiguration. It is
	// the ledger key on ServiceEntitlement.status.provisionedResources, so it
	// is what a consumer sees when a resource does not arrive, and it scopes
	// pruning, so renaming a declaration reinstalls rather than edits.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Objects are the objects to install, written out in full.
	//
	// Each is stored as authored and applied into every entitled project under
	// the name it carries. There are no variables, no per-project values, and
	// no field paths: a provider that wants a different object writes a
	// different object. The API server validates each entry as an embedded
	// resource, so a missing or malformed apiVersion, kind, or metadata is
	// refused with no webhook in the picture. Whether the object is acceptable
	// is the owning API's decision, made when it accepts or refuses the write.
	//
	// The cap bounds one declaration's fan-out into one project.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	Objects []ProvisionedObject `json:"objects"`
}

// ProvisionedObject is a Kubernetes object embedded verbatim.
type ProvisionedObject struct {
	// +kubebuilder:validation:EmbeddedResource
	// +kubebuilder:pruning:PreserveUnknownFields
	runtime.RawExtension `json:",inline"`
}

// ServiceConfigurationStatus defines the observed state of a
// ServiceConfiguration. The controller records compact top-level
// conditions here; per-item status lives on the downstream billing
// objects themselves.
type ServiceConfigurationStatus struct {
	// CatalogStatus embeds the shared catalog lifecycle fields
	// (publishedAt, conditions, observedGeneration).
	CatalogStatus `json:",inline"`

	// ServiceName is the resolved canonical reverse-DNS name of the
	// referenced Service (e.g. "compute.datumapis.com"). Populated by
	// the controller after the serviceRef is successfully resolved.
	//
	// +kubebuilder:validation:Optional
	ServiceName string `json:"serviceName,omitempty"`
}

// ServiceConfiguration is the Schema for the serviceconfigurations API.
// It is the single provider-facing document that declares everything a
// service contributes to Milo beyond its identity record. metadata.name
// is conventionally the service's reverse-DNS slug (e.g.
// "compute-miloapis-com") to make the 1:1 relationship between Service
// and ServiceConfiguration obvious at a glance.
//
// +genclient
// +genclient:nonNamespaced
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.status.serviceName`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.spec.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Platform"
type ServiceConfiguration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServiceConfigurationSpec   `json:"spec,omitempty"`
	Status ServiceConfigurationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ServiceConfigurationList contains a list of ServiceConfiguration.
type ServiceConfigurationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ServiceConfiguration `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ServiceConfiguration{}, &ServiceConfigurationList{})
}
