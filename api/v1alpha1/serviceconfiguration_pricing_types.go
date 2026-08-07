// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

// PricingRateEntry is a single Usage rate. Exactly one of Flat or Tiered
// must be set. An optional Match filters the rate by dimension value; the
// last unmatched entry is the default catch-all in Milo.
//
// Note: the Amberflo provider cannot materialize mixed match + unmatched
// catch-all rates today (DimensionMatrixNode has no default bucket). When
// targeting that backend, price every dimension value explicitly — use a
// sentinel match value the meter emits (e.g. "other") for fallbacks.
//
// +kubebuilder:validation:XValidation:rule="has(self.flat) != has(self.tiered)",message="exactly one of flat or tiered must be set"
type PricingRateEntry struct {
	// Match optionally restricts this rate to a single dimension value.
	//
	// +kubebuilder:validation:Optional
	Match *DimensionMatch `json:"match,omitempty"`

	// Flat is a single decimal USD string multiplied by metered usage.
	// Mutually exclusive with Tiered.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Pattern=`^(0|[1-9]\d*)(\.\d+)?$`
	Flat string `json:"flat,omitempty"`

	// Tiered is an ordered list of graduated volume bands.
	// Mutually exclusive with Flat. The last band must omit upTo; the
	// admission webhook enforces that (CEL last-index checks exceed the
	// CRD validation cost budget).
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	Tiered []PricingTierBand `json:"tiered,omitempty"`
}

// DimensionMatch selects a rate by a single meter dimension value.
// Multi-dimension matches are deferred.
type DimensionMatch struct {
	// Dimension is the label key declared on the metric (e.g. "tier",
	// "region", "model").
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Dimension string `json:"dimension"`

	// Value is the dimension value this rate applies to.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Value string `json:"value"`
}

// PricingTierBand is a single graduated volume band. Aggregation for tier
// breaks is monthly at billing-account scope. Each band has a rate and an
// exclusive upper bound upTo in pricingUnit units. The last band omits
// upTo (open-ended).
type PricingTierBand struct {
	// UpTo is the exclusive upper bound of this band in pricingUnit units.
	// Required on every band except the last, which must omit it.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Pattern=`^(0|[1-9]\d*)(\.\d+)?$`
	UpTo string `json:"upTo,omitempty"`

	// Rate is the USD decimal string applied to usage within this band.
	// Zero ("0") is valid for free allowances.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^(0|[1-9]\d*)(\.\d+)?$`
	Rate string `json:"rate"`
}

// ServiceChargeType distinguishes Usage, OneTime, and Recurring charges
// declared on a ServiceConfiguration. Metrics stay telemetry/quota-only;
// all commercial terms live on spec.charges[].
//
// +kubebuilder:validation:Enum=Usage;OneTime;Recurring
type ServiceChargeType string

const (
	// ServiceChargeTypeUsage meters consumption against a named metric.
	ServiceChargeTypeUsage ServiceChargeType = "Usage"

	// ServiceChargeTypeOneTime is a fixed amount charged once at a defined trigger.
	ServiceChargeTypeOneTime ServiceChargeType = "OneTime"

	// ServiceChargeTypeRecurring is a fixed amount charged each billing cycle.
	ServiceChargeTypeRecurring ServiceChargeType = "Recurring"
)

// ChargeTrigger identifies when a OneTime charge fires.
//
// +kubebuilder:validation:Enum=BillingAccountActivation
type ChargeTrigger string

const (
	// ChargeTriggerBillingAccountActivation fires when a billing account
	// first becomes active on the service.
	ChargeTriggerBillingAccountActivation ChargeTrigger = "BillingAccountActivation"
)

// ChargeInterval is the cadence for a Recurring charge.
//
// +kubebuilder:validation:Enum=monthly
type ChargeInterval string

const (
	// ChargeIntervalMonthly bills once per calendar month.
	ChargeIntervalMonthly ChargeInterval = "monthly"
)

// UsageChargeOptions holds fields that apply only when chargeType is Usage.
type UsageChargeOptions struct {
	// MetricRef is the full metric name this charge prices. Must match a
	// spec.metrics[].name.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	MetricRef string `json:"metricRef"`

	// PricingUnit is a human-readable billing unit label (e.g. "vcpu",
	// "gib"). Does not need to be the literal UCUM unit string of the meter.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	PricingUnit string `json:"pricingUnit"`

	// Rates is the ordered list of rate entries for this Usage charge.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +listType=atomic
	Rates []PricingRateEntry `json:"rates"`
}

// OneTimeChargeOptions holds fields that apply only when chargeType is OneTime.
type OneTimeChargeOptions struct {
	// Amount is the fixed USD decimal string.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^(0|[1-9]\d*)(\.\d+)?$`
	Amount string `json:"amount"`

	// Trigger identifies when this OneTime charge fires.
	//
	// +kubebuilder:validation:Required
	Trigger ChargeTrigger `json:"trigger"`
}

// RecurringChargeOptions holds fields that apply only when chargeType is Recurring.
type RecurringChargeOptions struct {
	// Amount is the fixed USD decimal string.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^(0|[1-9]\d*)(\.\d+)?$`
	Amount string `json:"amount"`

	// Interval is the cadence for this Recurring charge.
	//
	// +kubebuilder:validation:Required
	Interval ChargeInterval `json:"interval"`
}

// ServiceChargeSpec declares a Usage, OneTime, or Recurring charge for a
// service. Fans out to a ServicePricing with the matching chargeType.
// Metrics are referenced by name for Usage charges and are never priced
// in-place on the metric itself. Type-specific fields live under usage,
// oneTime, or recurring so supported options are explicit per chargeType.
//
// +kubebuilder:validation:XValidation:rule="self.chargeType != 'Usage' || (has(self.usage) && !has(self.oneTime) && !has(self.recurring))",message="Usage charges require usage and must not set oneTime or recurring"
// +kubebuilder:validation:XValidation:rule="self.chargeType != 'OneTime' || (has(self.oneTime) && !has(self.usage) && !has(self.recurring))",message="OneTime charges require oneTime and must not set usage or recurring"
// +kubebuilder:validation:XValidation:rule="self.chargeType != 'Recurring' || (has(self.recurring) && !has(self.usage) && !has(self.oneTime))",message="Recurring charges require recurring and must not set usage or oneTime"
type ServiceChargeSpec struct {
	// Name uniquely identifies this charge within the ServiceConfiguration
	// and must be prefixed with the Service's canonical serviceName (same
	// rule as metrics) so ServicePricing metadata.name stays unique in
	// milo-system. Immutable once the ServiceConfiguration is Published.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// ChargeType is Usage, OneTime, or Recurring.
	//
	// +kubebuilder:validation:Required
	ChargeType ServiceChargeType `json:"chargeType"`

	// DisplayName is a human-readable label for invoices and portals.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=128
	DisplayName string `json:"displayName,omitempty"`

	// Currency is the ISO 4217 currency code. USD only in v1.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Pattern=`^USD$`
	// +kubebuilder:default=USD
	Currency string `json:"currency,omitempty"`

	// Usage holds Usage-specific options. Required when chargeType is Usage.
	//
	// +kubebuilder:validation:Optional
	Usage *UsageChargeOptions `json:"usage,omitempty"`

	// OneTime holds OneTime-specific options. Required when chargeType is OneTime.
	//
	// +kubebuilder:validation:Optional
	OneTime *OneTimeChargeOptions `json:"oneTime,omitempty"`

	// Recurring holds Recurring-specific options. Required when chargeType is Recurring.
	//
	// +kubebuilder:validation:Optional
	Recurring *RecurringChargeOptions `json:"recurring,omitempty"`
}

// QuotaGatingMode controls whether quota for a service is gated on the
// account's active BillingEntitlement Offer.
//
// +kubebuilder:validation:Enum=OrganizationDefault;BillingEntitlement
type QuotaGatingMode string

const (
	// QuotaGatingOrganizationDefault grants quota from organization
	// defaults without requiring a BillingEntitlement.
	QuotaGatingOrganizationDefault QuotaGatingMode = "OrganizationDefault"

	// QuotaGatingBillingEntitlement gates quota on the account's active
	// BillingEntitlement Offer.
	QuotaGatingBillingEntitlement QuotaGatingMode = "BillingEntitlement"
)
