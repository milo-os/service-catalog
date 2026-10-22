// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	resourcemanagerv1alpha1 "go.miloapis.com/milo/pkg/apis/resourcemanager/v1alpha1"
)

const projectLifecycleRetryInterval = 5 * time.Second

// getConsumerProject reads the root Project that owns a project-scoped control
// plane.
//
// A project-scoped controller can briefly remain engaged while the root
// project purger is already draining its virtual control plane. Starting new
// work during that window races the purge and produces retries against
// namespaces and APIs that have already disappeared. A nil result means the
// root Project is already gone and callers must not begin new setup work.
func getConsumerProject(
	ctx context.Context,
	rootClient client.Reader,
	rootAPIReader client.Reader,
	projectName string,
) (*resourcemanagerv1alpha1.Project, error) {
	var project resourcemanagerv1alpha1.Project
	if err := rootClient.Get(ctx, client.ObjectKey{Name: projectName}, &project); err != nil {
		if apierrors.IsNotFound(err) {
			if rootAPIReader == nil {
				rootAPIReader = rootClient
			}
			if err := rootAPIReader.Get(ctx, client.ObjectKey{Name: projectName}, &project); err != nil {
				if apierrors.IsNotFound(err) {
					return nil, nil
				}
				return nil, fmt.Errorf("confirm Project %q after cache miss: %w", projectName, err)
			}
			return &project, nil
		}
		return nil, fmt.Errorf("get Project %q: %w", projectName, err)
	}
	return &project, nil
}
