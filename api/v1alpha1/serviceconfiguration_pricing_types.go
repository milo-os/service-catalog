// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

// PricingRateEntry is a single Usage rate. Exactly one of Flat or Tiered
// must be set. An optional Match filters the rate by dimension value; the
// last unmatched entry is the default catch-all.
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
	// Mutually exclusive with Flat.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MinItems=1
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
	// Omit on the last band for an open-ended range.
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

// ServiceChargeSpec declares a Usage, OneTime, or Recurring charge for a
// service. Fans out to a ServicePricing with the matching chargeType.
// Metrics are referenced by name for Usage charges and are never priced
// in-place on the metric itself.
//
// +kubebuilder:validation:XValidation:rule="self.chargeType != 'Usage' || (has(self.metricRef) && has(self.pricingUnit) && has(self.rates))",message="Usage charges require metricRef, pricingUnit, and rates"
// +kubebuilder:validation:XValidation:rule="self.chargeType != 'OneTime' || (has(self.amount) && has(self.trigger))",message="OneTime charges require amount and trigger"
// +kubebuilder:validation:XValidation:rule="self.chargeType != 'Recurring' || (has(self.amount) && has(self.interval))",message="Recurring charges require amount and interval"
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

	// MetricRef is the full metric name this Usage charge prices. Must
	// match a spec.metrics[].name. Required when chargeType is Usage.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=253
	MetricRef string `json:"metricRef,omitempty"`

	// PricingUnit is a human-readable billing unit label (e.g. "vcpu",
	// "gib"). Required when chargeType is Usage. Does not need to be the
	// literal UCUM unit string of the meter.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=64
	PricingUnit string `json:"pricingUnit,omitempty"`

	// Rates is the ordered list of rate entries for Usage charges.
	// Required when chargeType is Usage.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MinItems=1
	// +listType=atomic
	Rates []PricingRateEntry `json:"rates,omitempty"`

	// Amount is the fixed USD decimal string for OneTime and Recurring
	// charges.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Pattern=`^(0|[1-9]\d*)(\.\d+)?$`
	Amount string `json:"amount,omitempty"`

	// Trigger identifies when a OneTime charge fires. Required when
	// chargeType is OneTime.
	//
	// +kubebuilder:validation:Optional
	Trigger ChargeTrigger `json:"trigger,omitempty"`

	// Interval is the cadence for a Recurring charge. Required when
	// chargeType is Recurring.
	//
	// +kubebuilder:validation:Optional
	Interval ChargeInterval `json:"interval,omitempty"`
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
