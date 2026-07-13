// Package activation implements the client-side UX for enabling a Datum
// Cloud service via the service catalog's Service/ServiceEntitlement APIs.
//
// It hosts the logic next to the API it interprets: service-catalog owns
// Service and ServiceEntitlement, so it also owns the code that classifies
// their state and drives a consumer through requesting and checking access.
// CLI tools (datumctl and any future caller) import this package rather than
// hand-rolling their own copy of the same flow.
//
// The package is service-agnostic. Single-service callers (Gate, Requester,
// Observe) take a ServiceInfo identifying one service (object name, canonical
// name, display noun, description, enablement mode) built from a live Service
// via NewServiceInfo. Catalog-wide callers (JoinCatalog) work from a
// ServiceList and a ServiceEntitlementList. Everything else — the
// entitlement state model, prompt/TTY/exit-code conventions, wait mechanics,
// and the user-facing copy — lives here so consumers converge instead of
// forking the flow.
package activation
