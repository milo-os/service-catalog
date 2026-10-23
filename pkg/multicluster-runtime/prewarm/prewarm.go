// SPDX-License-Identifier: AGPL-3.0-only

// Package prewarm starts a project cache's informers before the project is
// engaged with a multicluster manager, so engaging it does not list each
// watched kind one after another.
package prewarm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// InformerTimeout bounds how long a project's engagement waits for the
// informers started ahead of it. Past it, engagement carries on exactly as it
// would have without prewarming, and any informer still syncing is waited on
// there instead.
var InformerTimeout = 30 * time.Second

// WatchedKinds remembers every kind a provider's controllers have asked a
// project cache for, so the next project's cache can start and sync those
// informers before the project is engaged.
//
// Engaging a project hands it to every controller in turn, and
// multicluster-runtime starts each controller's watch on that project while
// holding the controller's lock, blocking until the informer has listed. So
// one slow list holds up that controller for every other project, and each
// project lists its watched kinds one controller at a time. After a restart
// the services leader engaged staging's 355 projects at about one a second,
// and the compute leader took about 9s to engage one new project. Listing
// before Engage, on the engaging goroutine itself, starts every kind's list at
// once; inside the lock only an already-synced informer is left to pick up.
//
// A provider uses it by adding ClusterOption to the options it builds project
// clusters with, and calling the project cache's WaitForCacheSync after
// starting it and before Engage. Kinds are learned from the controllers'
// own GetInformer calls, so a replica whose controllers are not running (a
// non-leader) never records a kind and starts no extra informers.
//
// The first project a provider engages after a restart is not prewarmed: no
// kind has been recorded yet, so its controllers list it during Engage as
// they would without this package. Every later project is prewarmed.
//
// Each provider keeps its own set, so a project cache only starts informers
// its own manager's controllers watch.
type WatchedKinds struct {
	mu    sync.Mutex
	kinds map[string]client.Object
}

// NewWatchedKinds returns an empty set of watched kinds. Each provider needs
// its own.
func NewWatchedKinds() *WatchedKinds {
	return &WatchedKinds{kinds: map[string]client.Object{}}
}

// ClusterOption wraps each project cache so it records watched kinds and
// prewarms them when the provider waits for it to sync.
func (w *WatchedKinds) ClusterOption() cluster.Option {
	return func(o *cluster.Options) {
		newCache := o.NewCache
		if newCache == nil {
			newCache = cache.New
		}
		o.NewCache = func(cfg *rest.Config, opts cache.Options) (cache.Cache, error) {
			c, err := newCache(cfg, opts)
			if err != nil {
				return nil, err
			}
			return &prewarmingCache{Cache: c, kinds: w, scheme: opts.Scheme}, nil
		}
	}
}

func (w *WatchedKinds) record(obj client.Object, scheme *runtime.Scheme) {
	gvk, err := apiutil.GVKForObject(obj, scheme)
	if err != nil {
		return
	}
	// The Go type is part of the key: a metadata-only watch of a kind is a
	// separate informer from a typed one.
	key := fmt.Sprintf("%T/%s", obj, gvk)

	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.kinds[key]; !ok {
		w.kinds[key] = obj.DeepCopyObject().(client.Object)
	}
}

func (w *WatchedKinds) snapshot() []client.Object {
	w.mu.Lock()
	defer w.mu.Unlock()
	objs := make([]client.Object, 0, len(w.kinds))
	for _, obj := range w.kinds {
		objs = append(objs, obj.DeepCopyObject().(client.Object))
	}
	return objs
}

// prewarmingCache is a project cache whose WaitForCacheSync first starts an
// informer for every kind recorded in kinds. A provider calls
// WaitForCacheSync after starting a project's cache and before engaging it.
type prewarmingCache struct {
	cache.Cache
	kinds  *WatchedKinds
	scheme *runtime.Scheme
}

func (c *prewarmingCache) GetInformer(ctx context.Context, obj client.Object, opts ...cache.InformerGetOption) (cache.Informer, error) {
	c.kinds.record(obj, c.scheme)
	return c.Cache.GetInformer(ctx, obj, opts...)
}

func (c *prewarmingCache) WaitForCacheSync(ctx context.Context) bool {
	log := logf.FromContext(ctx)
	for _, obj := range c.kinds.snapshot() {
		if _, err := c.Cache.GetInformer(ctx, obj, cache.BlockUntilSynced(false)); err != nil {
			// The controller that watches this kind reports the same error
			// when it engages the project; nothing to add here.
			log.V(1).Info("not prewarming informer", "kind", fmt.Sprintf("%T", obj), "error", err.Error())
		}
	}

	syncCtx, cancel := context.WithTimeout(ctx, InformerTimeout)
	defer cancel()
	if c.Cache.WaitForCacheSync(syncCtx) {
		return true
	}
	if ctx.Err() != nil || !errors.Is(syncCtx.Err(), context.DeadlineExceeded) {
		return false
	}
	// Some informer has not synced yet. Without prewarming nothing would
	// have been started here at all, so report synced and let engagement
	// wait on that informer as it always has.
	log.Info("prewarmed informers still syncing; engaging anyway", "timeout", InformerTimeout.String())
	return true
}
