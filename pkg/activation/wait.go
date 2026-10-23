package activation

import (
	"context"
	"errors"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/watch"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const (
	// firstStatusTimeout bounds the post-create wait for the platform's first
	// status write. Timing out is not an error — it resolves to Processing.
	firstStatusTimeout = 30 * time.Second

	// reentryWaitWindow is how young a Processing entitlement may be for the gate
	// to re-enter the bounded wait rather than reporting immediately. A wedged
	// operator must not become a per-invocation tax.
	reentryWaitWindow = 2 * time.Minute

	// defaultApprovalTimeout bounds `--wait` by default. Approval is a manual
	// provider step with no time bound, so this is a safety ceiling, not an SLA.
	defaultApprovalTimeout = 30 * time.Minute

	// approvalRereadInterval re-reads the object periodically during `--wait` to
	// cover a deployed operator that predates the pending-requeue hardening.
	approvalRereadInterval = 60 * time.Second

	// rewatchMinDelay and rewatchMaxDelay bound how soon `--wait` re-reads and
	// reopens a watch the server closed. Some API servers end a watch after
	// its first event, so waiting a full re-read interval would turn a setup
	// that finishes in a second into a minute's wait. The delay doubles while
	// watches keep closing without delivering a change, so a server that
	// can't watch at all degrades to polling rather than a hot loop.
	rewatchMinDelay = 100 * time.Millisecond
	rewatchMaxDelay = 5 * time.Second
)

// waitForFirstStatus watches a single entitlement for its first status write,
// bounded by firstStatusTimeout. It returns the first non-Processing state, or —
// on watch timeout, closure, or error — the result of a single re-read. A
// timeout therefore surfaces as Processing, not an error.
func waitForFirstStatus(ctx context.Context, ec EntitlementClient, name, resourceVersion string) (State, *servicesv1alpha1.ServiceEntitlement, error) {
	watchCtx, cancel := context.WithTimeout(ctx, firstStatusTimeout)
	defer cancel()

	w, err := ec.Watch(watchCtx, name, resourceVersion)
	if err != nil {
		return getAndClassify(ctx, ec, name)
	}
	defer w.Stop()

	for {
		select {
		case <-watchCtx.Done():
			return getAndClassify(ctx, ec, name)
		case ev, ok := <-w.ResultChan():
			if !ok {
				return getAndClassify(ctx, ec, name)
			}
			e, matched := entitlementFromEvent(ev, name)
			if !matched {
				continue
			}
			if st := Classify(Observation{Entitlement: e}); st != StateProcessing {
				return st, e, nil
			}
		}
	}
}

// waitUntilResolved blocks until the entitlement reaches a resolved state
// (Active, Denied, Revoked, or Unavailable) or ctx expires. It prefers a watch
// but re-reads on approvalRereadInterval so a stale-pending operator still
// converges. On ctx deadline it returns timedOut=true with the last-known state;
// on cancellation it returns ctx.Err().
func waitUntilResolved(ctx context.Context, ec EntitlementClient, name string) (state State, e *servicesv1alpha1.ServiceEntitlement, timedOut bool, err error) {
	state, e, err = getAndClassify(ctx, ec, name)
	if err != nil {
		return state, e, false, err
	}
	if isResolved(state, e) {
		return state, e, false, nil
	}

	ticker := time.NewTicker(approvalRereadInterval)
	defer ticker.Stop()
	rewatchDelay := rewatchMinDelay
	// seen is the last resourceVersion observed. A reopened watch replays the
	// current object as ADDED, so only a newer version counts as progress.
	seen := ""
	if e != nil {
		seen = e.ResourceVersion
	}

	for {
		w, werr := ec.Watch(ctx, name, "")
		var events <-chan watch.Event
		if werr == nil {
			events = w.ResultChan()
		}

		reestablish := false
		for !reestablish {
			select {
			case <-ctx.Done():
				if w != nil {
					w.Stop()
				}
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return state, e, true, nil
				}
				return state, e, false, ctx.Err()

			case <-ticker.C:
				st, ent, gerr := getAndClassify(ctx, ec, name)
				if gerr == nil {
					state, e = st, ent
					if isResolved(state, e) {
						if w != nil {
							w.Stop()
						}
						return state, e, false, nil
					}
				}

			case ev, ok := <-events:
				if !ok {
					// The server closed the watch. Re-read so a change made
					// while no watch was open isn't missed, then reopen it.
					w.Stop()
					reestablish = true
					continue
				}
				ent, matched := entitlementFromEvent(ev, name)
				if !matched {
					continue
				}
				if ent.ResourceVersion != seen {
					seen = ent.ResourceVersion
					rewatchDelay = rewatchMinDelay
				}
				state, e = Classify(Observation{Entitlement: ent}), ent
				if isResolved(state, e) {
					w.Stop()
					return state, e, false, nil
				}
			}
		}

		if werr == nil {
			select {
			case <-ctx.Done():
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return state, e, true, nil
				}
				return state, e, false, ctx.Err()
			case <-time.After(rewatchDelay):
			}
			rewatchDelay = min(2*rewatchDelay, rewatchMaxDelay)
			st, ent, gerr := getAndClassify(ctx, ec, name)
			if gerr == nil {
				state, e = st, ent
				if ent != nil {
					seen = ent.ResourceVersion
				}
				if isResolved(state, e) {
					return state, e, false, nil
				}
			}
			continue
		}

		// No watch available: wait out one re-read interval before retrying so
		// the loop degrades to polling rather than spinning.
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return state, e, true, nil
			}
			return state, e, false, ctx.Err()
		case <-ticker.C:
			st, ent, gerr := getAndClassify(ctx, ec, name)
			if gerr == nil {
				state, e = st, ent
				if isResolved(state, e) {
					return state, e, false, nil
				}
			}
		}
	}
}

// isResolved reports whether a state will not change without user or provider
// action — the terminal outcomes plus Active.
func isResolved(s State, entitlement *servicesv1alpha1.ServiceEntitlement) bool {
	if s.Terminal() {
		return true
	}
	return s == StateActive && entitlement != nil &&
		apimeta.IsStatusConditionTrue(entitlement.Status.Conditions, servicesv1alpha1.ConditionTypeSetupComplete)
}

// entitlementFromEvent extracts a matching entitlement from a watch event,
// ignoring deletes, bookmarks, and events for other objects.
func entitlementFromEvent(ev watch.Event, name string) (*servicesv1alpha1.ServiceEntitlement, bool) {
	if ev.Type != watch.Added && ev.Type != watch.Modified {
		return nil, false
	}
	e, ok := ev.Object.(*servicesv1alpha1.ServiceEntitlement)
	if !ok || e.Name != name {
		return nil, false
	}
	return e, true
}
