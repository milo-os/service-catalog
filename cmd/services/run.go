// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"

	"golang.org/x/sync/errgroup"
)

// runUntilManagerStops runs the primary manager and every worker beside it
// until the manager stops, a worker fails, or ctx ends, and returns only after
// all of them have stopped, so no elected manager is cut off while it releases
// its lease.
func runUntilManagerStops(
	ctx context.Context,
	startManager func(context.Context) error,
	workers ...func(context.Context) error,
) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g, ctx := errgroup.WithContext(ctx)

	for _, work := range workers {
		g.Go(func() error {
			return ignoreCanceled(work(ctx))
		})
	}

	g.Go(func() error {
		defer cancel()
		return ignoreCanceled(startManager(ctx))
	})

	return g.Wait()
}

func ignoreCanceled(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
