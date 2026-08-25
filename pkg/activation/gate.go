package activation

import (
	"context"
	"fmt"
	"time"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// Gate is the preflight run before a gated command. On a TTY it may prompt to
// request access, submit, and wait for the platform's first answer; without one
// it never prompts and never mutates. Run returns nil to let the command
// proceed (the service is usable), or an *Error carrying the exit code.
type Gate struct {
	Service ServiceInfo
	Client  EntitlementClient
	IO      IOStreams
	Project string
}

// Run executes the preflight. An empty project is a no-op: there is nothing to
// gate on, and the command's own "no project" error is clearer.
func (g Gate) Run(ctx context.Context) error {
	if g.Project == "" {
		return nil
	}
	if err := g.Service.Validate(); err != nil {
		return err
	}

	state, e, err := Observe(ctx, g.Client, g.Service)
	if err != nil {
		return reportGeneric(g.IO, err, "checking %s access: %v", g.Service.noun(), err)
	}
	return g.handle(ctx, state, e)
}

func (g Gate) handle(ctx context.Context, state State, e *servicesv1alpha1.ServiceEntitlement) error {
	switch state {
	case StateActive:
		return nil
	case StateNotRequested:
		return g.handleNotRequested(ctx)
	case StateProcessing:
		return g.handleProcessing(ctx, e)
	case StatePendingApproval:
		return reportPendingReentry(g.IO, g.Service, g.Project, e)
	case StateDenied:
		return reportDenied(g.IO, g.Service, g.Project, e)
	case StateRevoked:
		return reportRevoked(g.IO, g.Service, g.Project, e)
	case StateUnavailable:
		return reportUnavailable(g.IO, g.Service, e)
	case StateCatalogUnavailable:
		return reportCatalogUnavailable(g.IO, g.Service)
	default:
		return reportGeneric(g.IO, nil, "%s access is in an unexpected state (%s)", g.Service.noun(), state)
	}
}

// handleNotRequested enables a self-service service outright; for a
// provider-gated one it prompts and submits on a TTY, and otherwise reports
// without mutating.
//
// The split is the enablement mode, not the caller. A self-service request
// cannot be refused — it enables the service there and then — so a confirmation
// only stands between the user and the command they already ran, and refusing
// it without a TTY makes every self-service command unusable from CI. A
// provider-gated request is worth confirming, because it goes to a human and
// can sit unapproved.
func (g Gate) handleNotRequested(ctx context.Context) error {
	// An unset mode reads as self-service, which is the API's default for
	// spec.enablementPolicy and what NewServiceInfo already fills in.
	if g.Service.EnablementMode != servicesv1alpha1.EnablementModeGatedByProvider {
		return g.submitAndWait(ctx, "")
	}

	if !g.IO.IsInputTTY() {
		return reportNotEnabledNonInteractive(g.IO, g.Service, g.Project)
	}

	_, _ = fmt.Fprintf(g.IO.Err, "%s is not enabled for project %q.\n", g.Service.DisplayName, g.Project)
	_, _ = fmt.Fprintf(g.IO.Err, "Enabling it needs approval from the team that provides %s.\n", g.Service.noun())
	ok, err := g.IO.promptYesNo("Would you like to request access?")
	if err != nil {
		return reportGeneric(g.IO, err, "reading prompt response: %v", err)
	}
	if !ok {
		return reportDeclined(g.IO, g.Service)
	}
	return g.submitAndWait(ctx, "")
}

// handleProcessing re-enters the bounded wait for a young entitlement, then
// reclassifies. An older one (a wedged operator) reports immediately so it does
// not tax every invocation.
func (g Gate) handleProcessing(ctx context.Context, e *servicesv1alpha1.ServiceEntitlement) error {
	if e != nil && time.Since(e.CreationTimestamp.Time) > reentryWaitWindow {
		return reportProcessingReentry(g.IO, g.Service, g.Project, e)
	}

	state, ent, err := g.waitFirstStatus(ctx, resourceVersionOf(e))
	if err != nil {
		return reportGeneric(g.IO, err, "waiting for %s status: %v", g.Service.noun(), err)
	}
	switch state {
	case StateActive:
		return nil
	case StatePendingApproval:
		return reportPendingReentry(g.IO, g.Service, g.Project, ent)
	case StateDenied:
		return reportDenied(g.IO, g.Service, g.Project, ent)
	case StateRevoked:
		return reportRevoked(g.IO, g.Service, g.Project, ent)
	case StateUnavailable:
		return reportUnavailable(g.IO, g.Service, ent)
	default:
		return reportProcessingReentry(g.IO, g.Service, g.Project, ent)
	}
}

// submitAndWait creates the entitlement, waits for the platform's first answer
// with visible progress, and branches on the result. On Active it prints the
// enabled line and returns nil so the original command runs.
func (g Gate) submitAndWait(ctx context.Context, message string) error {
	// Enabling a service on someone's project is a side effect they should see
	// happen, especially when nothing asked them first.
	if g.Service.EnablementMode == servicesv1alpha1.EnablementModeGatedByProvider {
		_, _ = fmt.Fprintf(g.IO.Err, "Requesting access to %s for project %q...\n", g.Service.noun(), g.Project)
	} else {
		_, _ = fmt.Fprintf(g.IO.Err, "Enabling %s for project %q...\n", g.Service.noun(), g.Project)
	}

	created, unavailable, cerr := createEntitlement(ctx, g.Client, g.Service, message)
	if cerr != nil {
		return reportGeneric(g.IO, cerr, "requesting %s access: %v", g.Service.noun(), cerr)
	}
	if unavailable {
		return reportUnavailable(g.IO, g.Service, nil)
	}

	state, ent, werr := g.waitFirstStatus(ctx, resourceVersionOf(created))
	if werr != nil {
		return reportGeneric(g.IO, werr, "waiting for %s status: %v", g.Service.noun(), werr)
	}
	switch state {
	case StateActive:
		_, _ = fmt.Fprintf(g.IO.Err, "\n%s is now enabled for project %q.\n", g.Service.DisplayName, g.Project)
		return nil
	case StatePendingApproval:
		return reportSubmittedPending(g.IO, g.Service, g.Project, ent)
	case StateDenied:
		return reportDenied(g.IO, g.Service, g.Project, ent)
	case StateRevoked:
		return reportRevoked(g.IO, g.Service, g.Project, ent)
	case StateUnavailable:
		return reportUnavailable(g.IO, g.Service, ent)
	default:
		return reportSubmittedProcessing(g.IO, g.Service, g.Project)
	}
}

// waitFirstStatus runs the bounded first-status wait behind a progress
// indicator on the gate's IO streams.
func (g Gate) waitFirstStatus(ctx context.Context, resourceVersion string) (State, *servicesv1alpha1.ServiceEntitlement, error) {
	var (
		state State
		ent   *servicesv1alpha1.ServiceEntitlement
	)
	err := g.IO.progress(ctx, g.IO.IsInputTTY(), "Waiting for the platform to process the request...", func(c context.Context) error {
		var e error
		state, ent, e = waitForFirstStatus(c, g.Client, g.Service.ObjectName, resourceVersion)
		return e
	})
	return state, ent, err
}
