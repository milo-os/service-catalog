// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

// MetricPricing optionally attaches Usage rates to a metric. Fans out to a
// ServicePricing with chargeType Usage.
type MetricPricing struct {
	// Currency is the ISO 4217 currency code. USD only in v1.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Pattern=`^USD$`
	// +kubebuilder:default=USD
	Currency string `json:"currency,omitempty"`

	// PricingUnit is a human-readable billing unit label (e.g. "vcpu",
	// "gib"). Does not need to be the literal UCUM unit string of the meter.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	PricingUnit string `json:"pricingUnit"`

	// Rates is the ordered list of rate entries for this metric.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +listType=atomic
	Rates []PricingRateEntry `json:"rates"`
}

// PricingRateEntry is a single rate. Exactly one of Flat or Tiered must be
// set. An optional Match filters the rate by dimension value; the last
// unmatched entry is the default catch-all.
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

// ServiceChargeType distinguishes OneTime and Recurring fixed charges
// declared on a ServiceConfiguration. Usage pricing lives on metrics.
//
// +kubebuilder:validation:Enum=OneTime;Recurring
type ServiceChargeType string

const (
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

// ServiceChargeSpec declares a fixed OneTime or Recurring charge for a
// service. Fans out to a ServicePricing with the matching chargeType.
//
// +kubebuilder:validation:XValidation:rule="self.chargeType != 'OneTime' || has(self.trigger)",message="OneTime charges require trigger"
// +kubebuilder:validation:XValidation:rule="self.chargeType != 'Recurring' || has(self.interval)",message="Recurring charges require interval"
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

	// ChargeType is OneTime or Recurring.
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

	// Amount is the fixed USD decimal string.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^(0|[1-9]\d*)(\.\d+)?$`
	Amount string `json:"amount"`

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
