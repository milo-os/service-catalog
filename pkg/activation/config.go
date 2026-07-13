package activation

import (
	"fmt"
	"strings"
)

// Config parameterizes the activation flow for one service. It carries only
// service identity and copy nouns; per-invocation values (project, client, IO
// streams) are supplied separately so a single Config can be shared. Callers
// build a Config via ConfigFromService rather than authoring one by hand.
type Config struct {
	// ObjectName is the Service's metadata.name. It is written to
	// spec.serviceRef.name on create (admission rejects the canonical name) and
	// is the object-name fallback when selecting the entitlement.
	ObjectName string

	// CanonicalName is the reverse-DNS service identity (e.g.
	// "compute.datumapis.com"). It is the preferred selection key: dependency-origin
	// entitlements carry it in spec.serviceRef.name, so matching on it avoids
	// mistaking a dependency entitlement for the direct one.
	CanonicalName string

	// DisplayName is the human noun for the service, capitalized for use at the
	// start of a sentence (e.g. "Compute"). Mid-sentence uses are lowercased.
	DisplayName string

	// SupportURL is an optional pointer shown when the service is unavailable on
	// this platform environment.
	SupportURL string
}

// Validate reports whether the required identity fields are set.
func (c Config) Validate() error {
	if c.ObjectName == "" {
		return fmt.Errorf("activation: Config.ObjectName is required")
	}
	if c.CanonicalName == "" {
		return fmt.Errorf("activation: Config.CanonicalName is required")
	}
	if c.DisplayName == "" {
		return fmt.Errorf("activation: Config.DisplayName is required")
	}
	return nil
}

// noun returns the display noun for mid-sentence use ("compute").
func (c Config) noun() string { return strings.ToLower(c.DisplayName) }

// enableCommand is the copy-pasteable command that submits an enablement
// request, computed from the service's own canonical identity rather than a
// per-caller field so the SDK never drifts out of sync with a hardcoded verb.
func (c Config) enableCommand() string { return "datumctl services enable " + c.CanonicalName }

// statusCommand is the copy-pasteable command that checks current status.
func (c Config) statusCommand() string { return "datumctl services status " + c.CanonicalName }
