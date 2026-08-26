// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	billingv1alpha1 "go.miloapis.com/billing/api/v1alpha1"
)

const (
	billingFanOutDeletePollInterval = 100 * time.Millisecond
	billingFanOutDeleteTimeout      = 30 * time.Second
)

// waitForBillingObjectGone polls until key is not found. Used after shrink-triggered
// deletes so server-side apply does not race a terminating object or hit subtractive
// update validation on a resource that still exists.
func waitForBillingObjectGone(ctx context.Context, c client.Client, key client.ObjectKey, obj client.Object) error {
	return wait.PollUntilContextTimeout(
		ctx,
		billingFanOutDeletePollInterval,
		billingFanOutDeleteTimeout,
		true,
		func(pollCtx context.Context) (bool, error) {
			err := c.Get(pollCtx, key, obj)
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			if err != nil {
				return false, err
			}
			return false, nil
		},
	)
}

func waitForMonitoredResourceTypeGone(ctx context.Context, c client.Client, name string) error {
	key := client.ObjectKey{Name: name}
	return waitForBillingObjectGone(ctx, c, key, &billingv1alpha1.MonitoredResourceType{})
}

func waitForMeterDefinitionGone(ctx context.Context, c client.Client, name string) error {
	key := client.ObjectKey{Name: name}
	return waitForBillingObjectGone(ctx, c, key, &billingv1alpha1.MeterDefinition{})
}
