// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
)

// goneProjectDeleteInterval spaces the deletes of ServiceConsumers whose
// Project is gone. Every delete wakes each consumer provider, and compute's
// provider recomputes its whole consumer set on a single worker for each
// event, so a burst of deletes after a restart would hold back its live work.
// At one delete every 3 s that worker stays mostly idle.
const goneProjectDeleteInterval = 3 * time.Second

// deletePacer spaces deletes with one token bucket. Each request books its
// slot once and keeps it until it acts: booking again on every requeue would
// place the request behind every slot booked since, so its turn would never
// come. A nil pacer does not pace, so reconcilers built without
// SetupWithManager still work.
type deletePacer struct {
	mu      sync.Mutex
	limiter *rate.Limiter
	now     func() time.Time
	slots   map[mcreconcile.Request]time.Time
}

func newDeletePacer(interval time.Duration, now func() time.Time) *deletePacer {
	return &deletePacer{
		limiter: rate.NewLimiter(rate.Every(interval), 1),
		now:     now,
		slots:   make(map[mcreconcile.Request]time.Time),
	}
}

// wait returns how long req must still wait for its slot, booking the slot on
// the first call. Zero or less means req may act now.
func (p *deletePacer) wait(req mcreconcile.Request) time.Duration {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	slot, ok := p.slots[req]
	if !ok {
		slot = now.Add(p.limiter.ReserveN(now, 1).DelayFrom(now))
		p.slots[req] = slot
	}
	return slot.Sub(now)
}

// forget drops req's slot once req has acted or no longer needs to, so the
// slots never outnumber the requests still waiting.
func (p *deletePacer) forget(req mcreconcile.Request) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.slots, req)
}
