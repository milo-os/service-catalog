// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"encoding/json"
	"slices"
	"sync"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// A project pass reads every entitlement in the project and writes the setup
// aggregate back to each of them, while half a dozen other controllers write
// the same objects' status during setup. Passing every one of those writes
// through made a single compute -> networking -> IPAM setup cost dozens of
// full-project passes, most of them re-reading state they had just written or
// reacting to fields they never read.
//
// The filters below drop exactly two kinds of event:
//
//   - entitlement updates that change nothing a pass reads, such as finalizers,
//     provisioned resources, lastProvisioningEvaluation and condition messages;
//   - this reconciler's own writes, recognized by content (setup aggregate) or
//     resourceVersion (managed projections), so an echo of a write the pass
//     already accounted for does not schedule another pass.
//
// Anything else passes, including a setup aggregate or projection someone else
// changed, which is drift the next pass repairs. Create and delete events of
// entitlements always pass.
//
// Dropping an echo is safe because every consequence of a write is computed in
// the pass that made it: aggregation walks dependencies before their parents,
// so a child's new SetupComplete is already visible to its ancestors. If that
// pass fails part-way it returns an error and is retried. If the process stops
// part-way, the ledger is lost with it: the next process, or the next replica
// to win the lease, starts its watches with an initial list that delivers a
// create event for every entitlement, so the project gets a full pass anyway.

// projectionWriteLedger remembers what this reconciler last wrote so its
// watches can recognize their own echoes. It is in-memory by design; see
// above for why losing it is safe.
type projectionWriteLedger struct {
	mu sync.Mutex
	// setup is the setup aggregate each entitlement was last reconciled to,
	// keyed by UID.
	setup map[types.UID]ledgerEntry
	// objects holds the resourceVersions of each managed projection's recent
	// writes, keyed by UID.
	objects map[types.UID]objectWrites
}

type ledgerEntry struct {
	project string
	value   string
}

// objectWriteHistory is how many of an object's writes are remembered. One
// pass writes an object at most twice (metadata/spec, then status), and both
// echoes may still be queued when the second write returns.
const objectWriteHistory = 2

type objectWrites struct {
	project string
	// versions are the resourceVersions of the most recent writes.
	versions []string
	// deleted is set once this reconciler deleted the object.
	deleted bool
}

func newProjectionWriteLedger() *projectionWriteLedger {
	return &projectionWriteLedger{
		setup:   make(map[types.UID]ledgerEntry),
		objects: make(map[types.UID]objectWrites),
	}
}

// recordSetup notes the setup aggregate a pass settled on for an entitlement.
// It is recorded before the write so the echo can't arrive first.
func (l *projectionWriteLedger) recordSetup(project string, entitlement *servicesv1alpha1.ServiceEntitlement) {
	if l == nil || entitlement.UID == "" {
		return
	}
	fingerprint := setupFingerprint(entitlement)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.setup[entitlement.UID] = ledgerEntry{project: project, value: fingerprint}
}

// retainSetup forgets entitlements of project that a pass no longer saw.
func (l *projectionWriteLedger) retainSetup(project string, entitlements []servicesv1alpha1.ServiceEntitlement) {
	if l == nil {
		return
	}
	present := make(map[types.UID]struct{}, len(entitlements))
	for i := range entitlements {
		present[entitlements[i].UID] = struct{}{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for uid, entry := range l.setup {
		if _, ok := present[uid]; entry.project == project && !ok {
			delete(l.setup, uid)
		}
	}
}

func (l *projectionWriteLedger) isOwnSetup(entitlement *servicesv1alpha1.ServiceEntitlement) bool {
	if l == nil {
		return false
	}
	fingerprint := setupFingerprint(entitlement)
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.setup[entitlement.UID]
	return ok && entry.value == fingerprint
}

// recordWrite notes the resourceVersion a write to a managed projection
// returned.
func (l *projectionWriteLedger) recordWrite(project string, obj client.Object) {
	if l == nil || obj.GetUID() == "" || obj.GetResourceVersion() == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	versions := append(l.objects[obj.GetUID()].versions, obj.GetResourceVersion())
	if len(versions) > objectWriteHistory {
		versions = versions[len(versions)-objectWriteHistory:]
	}
	l.objects[obj.GetUID()] = objectWrites{project: project, versions: versions}
}

// recordDelete notes that this reconciler is deleting a managed projection.
// It is recorded before the delete so the echo can't arrive first, and must be
// undone with forgetDelete if the delete fails.
func (l *projectionWriteLedger) recordDelete(project string, obj client.Object) {
	if l == nil || obj.GetUID() == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.objects[obj.GetUID()] = objectWrites{project: project, deleted: true}
}

// forgetDelete undoes recordDelete after a delete that did not happen. Left
// in place, the flag would make a later deletion by someone else look like
// this reconciler's own, and the projection would not be recreated.
func (l *projectionWriteLedger) forgetDelete(obj client.Object) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.objects[obj.GetUID()].deleted {
		delete(l.objects, obj.GetUID())
	}
}

func (l *projectionWriteLedger) isOwnWrite(obj client.Object) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Contains(l.objects[obj.GetUID()].versions, obj.GetResourceVersion())
}

// consumeOwnDelete reports whether this reconciler deleted obj, forgetting it
// either way: a deleted object's UID never comes back.
func (l *projectionWriteLedger) consumeOwnDelete(obj client.Object) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	deleted := l.objects[obj.GetUID()].deleted
	delete(l.objects, obj.GetUID())
	return deleted
}

// forgetProject drops everything recorded for a project that is gone or no
// longer engaged.
func (l *projectionWriteLedger) forgetProject(project string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for uid, entry := range l.setup {
		if entry.project == project {
			delete(l.setup, uid)
		}
	}
	for uid, entry := range l.objects {
		if entry.project == project {
			delete(l.objects, uid)
		}
	}
}

// setupFingerprint is the JSON of the fields a setup aggregate writes.
// Comparing serialized forms makes a value read back from the API server,
// whose timestamps have whole-second precision, equal the in-memory value it
// was written from.
func setupFingerprint(entitlement *servicesv1alpha1.ServiceEntitlement) string {
	raw, _ := json.Marshal(struct {
		Setup         *servicesv1alpha1.ServiceSetupStatus `json:"setup"`
		SetupComplete any                                  `json:"setupComplete"`
	}{
		Setup:         entitlement.Status.Setup,
		SetupComplete: apimeta.FindStatusCondition(entitlement.Status.Conditions, servicesv1alpha1.ConditionTypeSetupComplete),
	})
	return string(raw)
}

// projectionInputs is everything a pass reads from an entitlement, other than
// the setup aggregate it writes itself.
func projectionInputs(entitlement *servicesv1alpha1.ServiceEntitlement) string {
	type conditionInput struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
		// Ready's transition time becomes setup.authorizedAt.
		LastTransitionTime string `json:"lastTransitionTime"`
	}
	conditions := make(map[string]*conditionInput)
	for _, conditionType := range []string{
		servicesv1alpha1.ConditionTypeReady,
		servicesv1alpha1.ConditionTypeDependenciesSatisfied,
		servicesv1alpha1.ConditionTypeProvisioned,
	} {
		if c := apimeta.FindStatusCondition(entitlement.Status.Conditions, conditionType); c != nil {
			conditions[conditionType] = &conditionInput{
				Status: string(c.Status), Reason: c.Reason, LastTransitionTime: c.LastTransitionTime.UTC().Format(time.RFC3339),
			}
		}
	}
	raw, _ := json.Marshal(struct {
		Generation         int64                                `json:"generation"`
		Deleting           bool                                 `json:"deleting"`
		ServiceRef         string                               `json:"serviceRef"`
		ProviderActivation *servicesv1alpha1.ProviderActivation `json:"providerActivation"`
		Phase              servicesv1alpha1.EntitlementPhase    `json:"phase"`
		ServiceName        string                               `json:"serviceName"`
		Conditions         map[string]*conditionInput           `json:"conditions"`
		// Only whether Suspended is True is read: absent and False agree.
		Suspended bool `json:"suspended"`
	}{
		Generation:         entitlement.Generation,
		Deleting:           !entitlement.DeletionTimestamp.IsZero(),
		ServiceRef:         entitlement.Spec.ServiceRef.Name,
		ProviderActivation: entitlement.Spec.ProviderActivation,
		Phase:              entitlement.Status.Phase,
		ServiceName:        entitlement.Status.ServiceName,
		Conditions:         conditions,
		Suspended:          apimeta.IsStatusConditionTrue(entitlement.Status.Conditions, servicesv1alpha1.ConditionTypeSuspended),
	})
	return string(raw)
}

// entitlementProjectionPredicate passes entitlement updates that change a
// projection input, or change the setup aggregate to something this
// reconciler did not write.
func entitlementProjectionPredicate(ledger *projectionWriteLedger) predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldEntitlement, okOld := e.ObjectOld.(*servicesv1alpha1.ServiceEntitlement)
			newEntitlement, okNew := e.ObjectNew.(*servicesv1alpha1.ServiceEntitlement)
			if !okOld || !okNew {
				return true
			}
			if projectionInputs(oldEntitlement) != projectionInputs(newEntitlement) {
				return true
			}
			if setupFingerprint(oldEntitlement) == setupFingerprint(newEntitlement) {
				return false
			}
			return !ledger.isOwnSetup(newEntitlement)
		},
	}
}

// managedProjectionPredicate drops the echoes of this reconciler's own writes
// to projected Locations and mirrored ServiceAvailabilities. Any other change,
// including a deletion it did not make, passes as drift to repair.
func managedProjectionPredicate(ledger *projectionWriteLedger) predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return !ledger.isOwnWrite(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool { return !ledger.isOwnWrite(e.ObjectNew) },
		DeleteFunc: func(e event.DeleteEvent) bool { return !ledger.consumeOwnDelete(e.Object) },
	}
}
