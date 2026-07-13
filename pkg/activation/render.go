package activation

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// StatusReport is the machine-readable form emitted by the access status verb
// under -o json|yaml. It pairs the derived state enum with the raw entitlement
// so automation branches without scraping prose.
type StatusReport struct {
	Service     string                               `json:"service"`
	Project     string                               `json:"project"`
	State       State                                `json:"state"`
	Entitlement *servicesv1alpha1.ServiceEntitlement `json:"entitlement,omitempty"`
}

// NewStatusReport assembles a StatusReport from a classification result.
func NewStatusReport(cfg Config, project string, state State, e *servicesv1alpha1.ServiceEntitlement) StatusReport {
	return StatusReport{
		Service:     cfg.CanonicalName,
		Project:     project,
		State:       state,
		Entitlement: e,
	}
}

// RenderStatus writes the human-readable status block for the access verb to w.
// It never returns an error and never exits; state is data, not a failure.
func RenderStatus(w io.Writer, cfg Config, project string, state State, e *servicesv1alpha1.ServiceEntitlement) {
	fmt.Fprintf(w, "Service:  %s (%s)\n", cfg.DisplayName, cfg.CanonicalName)
	fmt.Fprintf(w, "Project:  %s\n", project)
	fmt.Fprintf(w, "Status:   %s\n", statusLine(cfg, state, e))
	if msg := serverMessage(state, e); msg != "" {
		fmt.Fprintf(w, "          %s\n", msg)
	}
	if hint := nextStepHint(cfg, state); hint != "" {
		fmt.Fprintf(w, "\n%s\n", hint)
	}
}

// CatalogReport is the machine-readable form emitted by the `services list`
// verb under -o json|yaml. It reuses StatusReport per entry so automation that
// already branches on a single service's report shape can reuse the same
// logic across a whole catalog listing.
type CatalogReport struct {
	Project  string         `json:"project"`
	Services []StatusReport `json:"services"`
}

// NewCatalogReport assembles a CatalogReport from a joined catalog listing.
func NewCatalogReport(project string, entries []CatalogEntry) CatalogReport {
	reports := make([]StatusReport, 0, len(entries))
	for _, entry := range entries {
		cfg := ConfigFromService(entry.Service)
		reports = append(reports, NewStatusReport(cfg, project, entry.State, entry.Entitlement))
	}
	return CatalogReport{Project: project, Services: reports}
}

// RenderList writes the human-readable catalog table for the `services list`
// verb to w: one row per published service, showing its current entitlement
// state for the active project.
func RenderList(w io.Writer, entries []CatalogEntry) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATE\tSINCE")
	for _, entry := range entries {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", entry.Service.DisplayName, entry.State, listSince(entry.Entitlement))
	}
	tw.Flush()
}

// listSince returns the age of the entitlement backing a catalog row, or ""
// when there is no entitlement to date the row from.
func listSince(e *servicesv1alpha1.ServiceEntitlement) string {
	if e == nil {
		return ""
	}
	return ageAgo(e.CreationTimestamp)
}

// statusLine is the one-line derived-state summary, with an age suffix where a
// timestamp gives one meaning.
func statusLine(cfg Config, state State, e *servicesv1alpha1.ServiceEntitlement) string {
	switch state {
	case StateActive:
		if e != nil && e.Status.EntitledAt != nil {
			return fmt.Sprintf("Active (enabled %s)", ageAgo(*e.Status.EntitledAt))
		}
		return "Active"
	case StatePendingApproval:
		return fmt.Sprintf("Pending approval (requested %s)", requestedAge(e))
	case StateProcessing:
		return fmt.Sprintf("Processing (requested %s)", requestedAge(e))
	case StateDenied:
		return "Denied"
	case StateRevoked:
		return "Revoked"
	case StateUnavailable:
		return "Unavailable"
	case StateCatalogUnavailable:
		return "Unavailable"
	case StateNotRequested:
		return "Not requested"
	default:
		return string(state)
	}
}

// nextStepHint is the single copy-pasteable command suggested for a state in the
// status view, or "" when none applies. It references only the enable/status
// verbs computed from the service's own identity.
func nextStepHint(cfg Config, state State) string {
	switch state {
	case StateNotRequested:
		return "Request access with: " + cfg.enableCommand()
	case StatePendingApproval, StateProcessing:
		return "Wait for activation: " + cfg.enableCommand() + " --wait"
	case StateDenied, StateRevoked:
		return "Request access again with: " + cfg.enableCommand() + " --renew"
	default:
		return ""
	}
}

// serverMessage returns the platform's Ready condition Message verbatim, or a
// state default when the condition carries none. The server explains "why"; the
// CLI must not paraphrase it.
func serverMessage(state State, e *servicesv1alpha1.ServiceEntitlement) string {
	if c := readyCondition(e); c != nil && c.Message != "" {
		return c.Message
	}
	switch state {
	case StateProcessing:
		return "The request is being processed."
	case StateNotRequested:
		return "This service is not enabled for this project."
	default:
		return ""
	}
}

// requestedAge returns the age of the entitlement's creation, or "just now".
func requestedAge(e *servicesv1alpha1.ServiceEntitlement) string {
	if e == nil {
		return "just now"
	}
	return ageAgo(e.CreationTimestamp)
}

// ageAgo formats a timestamp as a compact relative age with an "ago" suffix.
func ageAgo(t metav1.Time) string {
	if t.IsZero() {
		return "just now"
	}
	d := time.Since(t.Time)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
