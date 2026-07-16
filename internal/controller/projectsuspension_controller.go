// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	resourcemanagerv1alpha1 "go.miloapis.com/milo/pkg/apis/resourcemanager/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

// ProjectSuspensionPropagationReconciler propagates the Project's Suspension state
// to all ServiceConsumers associated with that project.
type ProjectSuspensionPropagationReconciler struct {
	client  client.Client
	Scheme  *runtime.Scheme
	Manager mcmanager.Manager
}

// +kubebuilder:rbac:groups=resourcemanager.miloapis.com,resources=projects,verbs=get;list;watch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceconsumers,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=services.miloapis.com,resources=serviceentitlements,verbs=get;list;watch

func (r *ProjectSuspensionPropagationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var project resourcemanagerv1alpha1.Project
	if err := r.client.Get(ctx, req.NamespacedName, &project); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get Project: %w", err)
	}

	// 1. Determine if project is suspended.
	isSuspended := false
	if cond := apimeta.FindStatusCondition(project.Status.Conditions, resourcemanagerv1alpha1.ProjectSuspended); cond != nil {
		isSuspended = cond.Status == metav1.ConditionTrue
	}

	// 2. Fetch the cluster client for the project.
	cluster, err := r.Manager.GetCluster(ctx, multicluster.ClusterName(project.Name))
	if err != nil {
		// Project cluster might not be engaged yet; skip or requeue
		logger.V(1).Info("project cluster not engaged yet, skipping propagation", "project", project.Name)
		return ctrl.Result{}, nil
	}
	consumerClient := cluster.GetClient()

	// 3. List all ServiceEntitlement resources in the project cluster.
	var entitlements servicesv1alpha1.ServiceEntitlementList
	if err := consumerClient.List(ctx, &entitlements); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list ServiceEntitlements: %w", err)
	}

	for i := range entitlements.Items {
		ent := &entitlements.Items[i]
		// A ServiceConsumer is upserted for every entitlement phase (Active,
		// PendingApproval, Rejected) — only skip entitlements that never got
		// far enough to have one.
		if ent.Status.Phase == "" {
			continue
		}

		// 4. Resolve the Service to find the provider project. Use the same
		// canonical-name-first lookup as the entitlement controller, since
		// spec.serviceRef.name may hold either the canonical service name or
		// the Kubernetes object name.
		svc, err := resolveService(ctx, r.client, ent.Spec.ServiceRef.Name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return ctrl.Result{}, fmt.Errorf("failed to get Service %q: %w", ent.Spec.ServiceRef.Name, err)
		}

		if svc.Spec.Phase == servicesv1alpha1.PhaseDraft {
			continue
		}

		providerProject := svc.Spec.Owner.ProducerProjectRef.Name
		providerCluster, err := r.Manager.GetCluster(ctx, multicluster.ClusterName(providerProject))
		if err != nil {
			logger.Info("provider cluster not engaged, skipping consumer updates", "providerProject", providerProject)
			continue
		}
		providerClient := providerCluster.GetClient()

		// 5. Update the ServiceConsumer spec.
		consumerName := serviceConsumerName(svc.Spec.ServiceName, project.Name)
		var consumer servicesv1alpha1.ServiceConsumer
		if err := providerClient.Get(ctx, client.ObjectKey{Name: consumerName}, &consumer); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return ctrl.Result{}, fmt.Errorf("failed to get ServiceConsumer %q: %w", consumerName, err)
		}

		if consumer.Spec.Suspended != isSuspended {
			before := consumer.DeepCopy()
			consumer.Spec.Suspended = isSuspended
			if err := providerClient.Patch(ctx, &consumer, client.MergeFrom(before)); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to patch ServiceConsumer %q: %w", consumerName, err)
			}
			logger.Info("propagated project suspension state to ServiceConsumer",
				"project", project.Name,
				"serviceConsumer", consumerName,
				"suspended", isSuspended,
			)
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler on the root manager.
func (r *ProjectSuspensionPropagationReconciler) SetupWithManager(mgr ctrl.Manager, mcMgr mcmanager.Manager) error {
	r.client = mgr.GetClient()
	r.Manager = mcMgr

	return ctrl.NewControllerManagedBy(mgr).
		For(&resourcemanagerv1alpha1.Project{}).
		Named("project-suspension-propagation").
		Complete(r)
}
