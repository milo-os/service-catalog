// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	testingclock "k8s.io/utils/clock/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	resourcemanagerv1alpha1 "go.miloapis.com/milo/pkg/apis/resourcemanager/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const (
	provConsumerProject = "consumer-proj"
	provSourceProject   = "networking-platform"
	provEntitlement     = "networking"
	provServiceName     = "networking.datumapis.com"
	provConfigName      = "networking-datumapis-com"
	provEntitlementUID  = types.UID("ent-networking-uid")
)

var (
	ipClassGVK = schema.GroupVersionKind{
		Group:   "ipam.miloapis.com",
		Version: "v1alpha1",
		Kind:    "IPClass",
	}
	ipPoolGVK = ipClassGVK.GroupVersion().WithKind("IPPool")
)

func provScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = servicesv1alpha1.AddToScheme(s)
	_ = resourcemanagerv1alpha1.AddToScheme(s)
	for _, gvk := range []schema.GroupVersionKind{ipClassGVK, ipPoolGVK} {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	return s
}

func provClient(objs ...client.Object) client.Client {
	hasProject := false
	for _, obj := range objs {
		if project, ok := obj.(*resourcemanagerv1alpha1.Project); ok && project.Name == provConsumerProject {
			hasProject = true
			break
		}
	}
	if !hasProject {
		objs = append(objs, &resourcemanagerv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: provConsumerProject}})
	}
	return provClientWithoutProject(objs...)
}

func provClientWithoutProject(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(provScheme()).
		WithObjects(objs...).
		Build()
}

// provConsumerClient keeps ServiceEntitlement's status subresource enabled so
// the ledger patch exercises the same path it does against a real API server.
func provConsumerClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(provScheme()).
		WithTypeConverters(fakeTypeConverters()...).
		WithObjects(objs...).
		WithStatusSubresource(&servicesv1alpha1.ServiceEntitlement{}).
		Build()
}

// blockingApplyClient holds the first object apply so a test can prove several
// projects enter the write path together. Each project gets its own client,
// matching the isolation provided by the multicluster manager in production.
type blockingApplyClient struct {
	client.Client
	project string
	entered chan<- string
	release <-chan struct{}
	once    sync.Once
}

func (c *blockingApplyClient) Apply(
	ctx context.Context,
	obj runtime.ApplyConfiguration,
	opts ...client.ApplyOption,
) error {
	c.once.Do(func() {
		c.entered <- c.project
		<-c.release
	})
	return c.Client.Apply(ctx, obj, opts...)
}

func provEntitlementObj(phase servicesv1alpha1.EntitlementPhase) *servicesv1alpha1.ServiceEntitlement {
	return &servicesv1alpha1.ServiceEntitlement{
		ObjectMeta: metav1.ObjectMeta{Name: provEntitlement, UID: provEntitlementUID},
		Spec: servicesv1alpha1.ServiceEntitlementSpec{
			ServiceRef: servicesv1alpha1.ServiceRef{Name: provServiceName},
		},
		Status: servicesv1alpha1.ServiceEntitlementStatus{Phase: phase},
	}
}

func provObject(body string) servicesv1alpha1.ProvisionedObject {
	return servicesv1alpha1.ProvisionedObject{RawExtension: runtime.RawExtension{Raw: []byte(body)}}
}

// The shape IPAM gives a cross-project class reference: spec.source, keyed by
// project and name. The provider writes it; the platform does not know it.
func ipClassRef(name string) servicesv1alpha1.ProvisionedObject {
	return provObject(fmt.Sprintf(`{
		"apiVersion": "ipam.miloapis.com/v1alpha1",
		"kind": "IPClass",
		"metadata": {"name": %q},
		"spec": {"source": {"project": %q, "name": %q}}
	}`, name, provSourceProject, name))
}

func provConfig(objects ...servicesv1alpha1.ProvisionedObject) *servicesv1alpha1.ServiceConfiguration {
	return &servicesv1alpha1.ServiceConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: provConfigName},
		Spec: servicesv1alpha1.ServiceConfigurationSpec{
			ServiceRef: servicesv1alpha1.ServiceReference{Name: provServiceName},
			Phase:      servicesv1alpha1.PhasePublished,
			Provisioning: &servicesv1alpha1.ServiceProvisioningConfig{
				Resources: []servicesv1alpha1.ProvisionedResourceSpec{{
					Name:    "address-classes",
					Objects: objects,
				}},
			},
		},
		Status: servicesv1alpha1.ServiceConfigurationStatus{ServiceName: provServiceName},
	}
}

func newProvReconciler(root client.Client, clusters map[string]client.Client) *ProvisioningReconciler {
	mgr := newTestManager()
	for name, c := range clusters {
		mgr.add(name, c)
	}
	return &ProvisioningReconciler{rootClient: root, rootAPIReader: root, Manager: mgr, Scheme: provScheme()}
}

func provReconcile(t *testing.T, r *ProvisioningReconciler) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), mcreconcile.Request{
		Request:     ctrl.Request{NamespacedName: types.NamespacedName{Name: provEntitlement}},
		ClusterName: provConsumerProject,
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

func listInstalled(t *testing.T, c client.Client, gvk schema.GroupVersionKind) *unstructured.UnstructuredList {
	t.Helper()
	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatalf("list installed %s: %v", gvk.Kind, err)
	}
	return &list
}

func listClasses(t *testing.T, c client.Client) *unstructured.UnstructuredList {
	t.Helper()
	return listInstalled(t, c, ipClassGVK)
}

func getEntitlement(t *testing.T, c client.Client) *servicesv1alpha1.ServiceEntitlement {
	t.Helper()
	var ent servicesv1alpha1.ServiceEntitlement
	if err := c.Get(context.Background(), types.NamespacedName{Name: provEntitlement}, &ent); err != nil {
		t.Fatalf("get entitlement: %v", err)
	}
	return &ent
}

func setConfig(t *testing.T, root client.Client, mutate func(*servicesv1alpha1.ServiceConfiguration)) {
	t.Helper()
	var live servicesv1alpha1.ServiceConfiguration
	if err := root.Get(context.Background(), types.NamespacedName{Name: provConfigName}, &live); err != nil {
		t.Fatalf("get config: %v", err)
	}
	mutate(&live)
	if err := root.Update(context.Background(), &live); err != nil {
		t.Fatalf("update config: %v", err)
	}
}

func TestProvisioningInstallsDeclaredObjectsForActiveEntitlement(t *testing.T) {
	root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6")))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	list := listClasses(t, consumer)
	if len(list.Items) != 1 {
		t.Fatalf("expected the declared class to be installed, got %d", len(list.Items))
	}

	got := list.Items[0]
	// The provider names the object; the platform installs it verbatim.
	if got.GetName() != "tenant-endpoint-ipv6" {
		t.Errorf("installed name = %q, want the name the declaration carries", got.GetName())
	}
	source, _, _ := unstructured.NestedMap(got.Object, "spec", "source")
	if source["project"] != provSourceProject || source["name"] != "tenant-endpoint-ipv6" {
		t.Errorf("installed spec.source = %+v, want the reference the provider wrote", source)
	}

	// Owner reference to the cluster-scoped entitlement is the teardown path
	// that does not depend on the project purger.
	if !ownedBy(got.GetOwnerReferences(), provEntitlementUID) {
		t.Errorf("installed object is not owned by the entitlement: %+v", got.GetOwnerReferences())
	}

	// Provenance moved entirely into the labels once the name stopped carrying
	// it, so this is what answers "which service put this here".
	labels := got.GetLabels()
	if labels[labelManagedBy] != labelManagedByValue ||
		labels[labelServiceName] != encodeName(provServiceName) ||
		labels[labelEntitlementName] != provEntitlement ||
		labels[labelProvisionedResource] != "address-classes" {
		t.Errorf("installed object does not record its provenance: %+v", labels)
	}

	entry := getEntitlement(t, consumer).Status.ProvisionedResources[0]
	if entry.State != servicesv1alpha1.ProvisionedResourceStateInstalled || entry.ObjectCount != 1 {
		t.Fatalf("ledger does not report the install: %+v", entry)
	}
	if len(entry.Kinds) != 1 || entry.Kinds[0].Kind != "IPClass" || entry.Kinds[0].Version != "v1alpha1" {
		t.Errorf("ledger does not record what was installed: %+v", entry.Kinds)
	}
}

func TestProvisioningDoesNotDropSetupOnProjectCacheMiss(t *testing.T) {
	cacheClient := provClientWithoutProject(provConfig(ipClassRef("tenant-endpoint-ipv6")))
	apiReader := provClient()
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))
	mgr := newTestManager()
	mgr.add(provConsumerProject, consumer)
	r := &ProvisioningReconciler{
		rootClient: cacheClient, rootAPIReader: apiReader, Manager: mgr, Scheme: provScheme(),
	}

	provReconcile(t, r)

	if list := listClasses(t, consumer); len(list.Items) != 1 {
		t.Fatalf("cache miss dropped provisioning; installed %d objects, want 1", len(list.Items))
	}
}

func TestProvisioningSkipsTerminatingProject(t *testing.T) {
	now := metav1.Now()
	project := &resourcemanagerv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{
		Name:              provConsumerProject,
		DeletionTimestamp: &now,
		Finalizers:        []string{"resourcemanager.miloapis.com/project"},
	}}
	root := provClient(project, provConfig(ipClassRef("tenant-endpoint-ipv6")))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	if list := listClasses(t, consumer); len(list.Items) != 0 {
		t.Fatalf("terminating project received %d provisioned objects", len(list.Items))
	}
	if got := getEntitlement(t, consumer); got.Status.LastProvisioningEvaluation != nil {
		t.Error("terminating project received a provisioning status write")
	}
}

func TestProvisioningPrunesInactiveEntitlementWhileProjectTerminates(t *testing.T) {
	root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6")))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))
	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)
	if got := len(listClasses(t, consumer).Items); got != 1 {
		t.Fatalf("precondition: installed objects = %d, want 1", got)
	}

	var project resourcemanagerv1alpha1.Project
	if err := root.Get(context.Background(), client.ObjectKey{Name: provConsumerProject}, &project); err != nil {
		t.Fatal(err)
	}
	project.Finalizers = []string{"test"}
	if err := root.Update(context.Background(), &project); err != nil {
		t.Fatal(err)
	}
	if err := root.Delete(context.Background(), &project); err != nil {
		t.Fatal(err)
	}

	entitlement := getEntitlement(t, consumer)
	entitlement.Status.Phase = servicesv1alpha1.EntitlementPhaseRejected
	if err := consumer.Status().Update(context.Background(), entitlement); err != nil {
		t.Fatal(err)
	}
	provReconcile(t, r)

	if got := len(listClasses(t, consumer).Items); got != 0 {
		t.Fatalf("terminating project retained %d explicitly provisioned objects", got)
	}
	condition := apimeta.FindStatusCondition(getEntitlement(t, consumer).Status.Conditions,
		servicesv1alpha1.ConditionTypeProvisioned)
	if condition == nil || condition.Reason != servicesv1alpha1.ReasonEntitlementNotActive {
		t.Fatalf("teardown status = %#v, want EntitlementNotActive", condition)
	}
}

func TestProvisioningSkipsMissingProject(t *testing.T) {
	root := provClientWithoutProject(provConfig(ipClassRef("tenant-endpoint-ipv6")))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))
	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})

	provReconcile(t, r)

	if list := listClasses(t, consumer); len(list.Items) != 0 {
		t.Fatalf("missing project received %d provisioned objects", len(list.Items))
	}
	if got := getEntitlement(t, consumer); got.Status.LastProvisioningEvaluation != nil {
		t.Error("missing project received a provisioning status write")
	}
}

func TestProvisioningRetriesDisengagedProject(t *testing.T) {
	r := newProvReconciler(provClient(), nil)
	result, err := r.Reconcile(context.Background(), mcreconcile.Request{
		Request:     ctrl.Request{NamespacedName: types.NamespacedName{Name: provEntitlement}},
		ClusterName: provConsumerProject,
	})
	if err != nil || result.RequeueAfter != projectLifecycleRetryInterval {
		t.Fatalf("reconcile result=%+v err=%v, want retry after %v", result, err, projectLifecycleRetryInterval)
	}
}

func TestProvisioningRetriesDisengagedTerminatingProject(t *testing.T) {
	now := metav1.Now()
	root := provClient(&resourcemanagerv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{
		Name: provConsumerProject, DeletionTimestamp: &now, Finalizers: []string{"test"},
	}})
	r := newProvReconciler(root, nil)
	result, err := r.Reconcile(context.Background(), mcreconcile.Request{
		Request:     ctrl.Request{NamespacedName: types.NamespacedName{Name: provEntitlement}},
		ClusterName: provConsumerProject,
	})
	if err != nil || result.RequeueAfter != projectLifecycleRetryInterval {
		t.Fatalf("reconcile result=%+v err=%v, want teardown retry after %v", result, err, projectLifecycleRetryInterval)
	}
}

// Reconciles for distinct projects must not share mutable state. The controller
// runs several workers so an API round-trip in one project cannot block every
// project behind it; this test holds all object writes until every project has
// reached the write path, then verifies each result kept its own owner.
func TestProvisioningReconcilesProjectsConcurrentlyAndInIsolation(t *testing.T) {
	const projectCount = provisioningMaxConcurrentReconciles

	rootObjects := []client.Object{provConfig(ipClassRef("tenant-endpoint-ipv6"))}
	entered := make(chan string, projectCount)
	release := make(chan struct{})
	consumers := make(map[string]client.Client, projectCount)
	managed := make(map[string]client.Client, projectCount)
	wantUID := make(map[string]types.UID, projectCount)

	for i := range projectCount {
		project := fmt.Sprintf("consumer-proj-%d", i)
		rootObjects = append(rootObjects, &resourcemanagerv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: project},
		})
		uid := types.UID(project + "-entitlement")
		entitlement := provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive)
		entitlement.UID = uid
		consumer := provConsumerClient(entitlement)
		consumers[project] = consumer
		wantUID[project] = uid
		managed[project] = &blockingApplyClient{
			Client:  consumer,
			project: project,
			entered: entered,
			release: release,
		}
	}

	root := provClient(rootObjects...)
	r := newProvReconciler(root, managed)
	errCh := make(chan error, projectCount)
	var wg sync.WaitGroup
	for project := range consumers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.Reconcile(context.Background(), mcreconcile.Request{
				Request:     ctrl.Request{NamespacedName: types.NamespacedName{Name: provEntitlement}},
				ClusterName: multicluster.ClusterName(project),
			})
			errCh <- err
		}()
	}

	seen := make(map[string]struct{}, projectCount)
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for len(seen) < projectCount {
		select {
		case project := <-entered:
			seen[project] = struct{}{}
		case <-timer.C:
			close(release)
			wg.Wait()
			t.Fatalf("only %d of %d projects reached the apply path concurrently", len(seen), projectCount)
		}
	}
	close(release)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent reconcile: %v", err)
		}
	}

	for project, consumer := range consumers {
		items := listClasses(t, consumer).Items
		if len(items) != 1 {
			t.Fatalf("project %q installed %d objects, want 1", project, len(items))
		}
		if !ownedBy(items[0].GetOwnerReferences(), wantUID[project]) {
			t.Errorf("project %q object has another project's owner: %+v", project, items[0].GetOwnerReferences())
		}
		entry := getEntitlement(t, consumer).Status.ProvisionedResources
		if len(entry) != 1 || entry[0].State != servicesv1alpha1.ProvisionedResourceStateInstalled {
			t.Errorf("project %q ledger did not converge: %+v", project, entry)
		}
	}
}

// A project that never enabled the service, or whose request has not been
// approved, must receive nothing. Provisioning follows approval.
func TestProvisioningSkipsEntitlementNotActive(t *testing.T) {
	for _, phase := range []servicesv1alpha1.EntitlementPhase{
		servicesv1alpha1.EntitlementPhasePendingApproval,
		servicesv1alpha1.EntitlementPhaseRejected,
	} {
		root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6")))
		consumer := provConsumerClient(provEntitlementObj(phase))

		r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
		provReconcile(t, r)

		if list := listClasses(t, consumer); len(list.Items) != 0 {
			t.Errorf("phase %s: expected nothing installed, got %d objects", phase, len(list.Items))
		}
		cond := apimeta.FindStatusCondition(getEntitlement(t, consumer).Status.Conditions,
			servicesv1alpha1.ConditionTypeProvisioned)
		if cond == nil || cond.Status != metav1.ConditionFalse ||
			cond.Reason != servicesv1alpha1.ReasonEntitlementNotActive {
			t.Errorf("phase %s: expected Provisioned=False/EntitlementNotActive, got %+v", phase, cond)
		}
	}
}

// Losing Active must remove what was installed, not merely stop adding to it.
func TestProvisioningPrunesWhenEntitlementLeavesActive(t *testing.T) {
	root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6")))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)
	if len(listClasses(t, consumer).Items) != 1 {
		t.Fatalf("precondition failed: nothing was installed")
	}

	ent := getEntitlement(t, consumer)
	ent.Status.Phase = servicesv1alpha1.EntitlementPhaseRejected
	if err := consumer.Status().Update(context.Background(), ent); err != nil {
		t.Fatalf("update entitlement phase: %v", err)
	}

	provReconcile(t, r)
	if list := listClasses(t, consumer); len(list.Items) != 0 {
		t.Errorf("expected installed objects to be pruned, got %d", len(list.Items))
	}
}

// Editing the declaration is how a provider changes what consumers hold, so the
// object it stops declaring has to go — including when the change is to the
// object's kind, which no longer appears in the declaration once removed.
func TestProvisioningConvergesOnDeclarationChange(t *testing.T) {
	root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6")))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)
	if got := listClasses(t, consumer).Items; len(got) != 1 {
		t.Fatalf("precondition failed, installed: %+v", names(got))
	}

	setConfig(t, root, func(sc *servicesv1alpha1.ServiceConfiguration) {
		sc.Spec.Provisioning.Resources[0].Objects = []servicesv1alpha1.ProvisionedObject{
			provObject(`{"apiVersion":"ipam.miloapis.com/v1alpha1","kind":"IPPool","metadata":{"name":"tenant-pool"}}`),
		}
	})
	provReconcile(t, r)

	if got := listClasses(t, consumer).Items; len(got) != 0 {
		t.Errorf("the withdrawn class survived a kind change: %+v", names(got))
	}
	if got := listInstalled(t, consumer, ipPoolGVK).Items; len(got) != 1 || got[0].GetName() != "tenant-pool" {
		t.Errorf("the new object did not arrive: %+v", names(got))
	}
}

// The declaration is applied, so a consumer edit to a field it states is
// reverted rather than left standing.
func TestProvisioningRestoresConsumerEdits(t *testing.T) {
	root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6")))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	edited := listClasses(t, consumer).Items[0]
	if err := unstructured.SetNestedMap(edited.Object, map[string]any{
		"project": "somewhere-else", "name": "somewhere-else",
	}, "spec", "source"); err != nil {
		t.Fatalf("edit installed object: %v", err)
	}
	if err := consumer.Update(context.Background(), &edited); err != nil {
		t.Fatalf("update installed object: %v", err)
	}

	provReconcile(t, r)
	source, _, _ := unstructured.NestedMap(listClasses(t, consumer).Items[0].Object, "spec", "source")
	if source["project"] != provSourceProject {
		t.Errorf("a consumer edit survived the next reconcile: %+v", source)
	}
}

// The shape of an embedded object has to hold in the controller even when the
// declaration is already in etcd, because admission can be bypassed, removed,
// or predate the schema that would have refused it.
func TestProvisioningRefusesObjectsItWillNotWrite(t *testing.T) {
	for _, tc := range []struct {
		name string
		obj  servicesv1alpha1.ProvisionedObject
	}{
		{"core group", provObject(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"x"}}`)},
		{"no version", provObject(`{"apiVersion":"ipam.miloapis.com","kind":"IPClass","metadata":{"name":"x"}}`)},
		{"no name", provObject(`{"apiVersion":"ipam.miloapis.com/v1alpha1","kind":"IPClass","metadata":{}}`)},
		{"namespaced", provObject(`{"apiVersion":"ipam.miloapis.com/v1alpha1","kind":"IPClass","metadata":{"name":"x","namespace":"kube-system"}}`)},
		{"owner reference", provObject(`{"apiVersion":"ipam.miloapis.com/v1alpha1","kind":"IPClass","metadata":{"name":"x","ownerReferences":[]}}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := provClient(provConfig(tc.obj))
			consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

			r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
			provReconcile(t, r)

			if list := listClasses(t, consumer); len(list.Items) != 0 {
				t.Fatalf("a refused declaration installed %d objects", len(list.Items))
			}
			ent := getEntitlement(t, consumer)
			if len(ent.Status.ProvisionedResources) != 1 {
				t.Fatalf("expected one ledger entry, got %+v", ent.Status.ProvisionedResources)
			}
			entry := ent.Status.ProvisionedResources[0]
			if entry.State != servicesv1alpha1.ProvisionedResourceStateUnprovisionable ||
				entry.Reason != reasonObjectInvalid {
				t.Errorf("expected Unprovisionable/ObjectInvalid, got %+v", entry)
			}
			cond := apimeta.FindStatusCondition(ent.Status.Conditions, servicesv1alpha1.ConditionTypeProvisioned)
			if cond == nil || cond.Status != metav1.ConditionFalse {
				t.Errorf("expected Provisioned=False, got %+v", cond)
			}
		})
	}
}

// A declaration that stops resolving must not erase the record teardown depends
// on, or its objects are stranded in every project that holds them.
func TestProvisioningKeepsTheLedgerWhenADeclarationStopsResolving(t *testing.T) {
	root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6")))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	setConfig(t, root, func(sc *servicesv1alpha1.ServiceConfiguration) {
		sc.Spec.Provisioning.Resources[0].Objects = []servicesv1alpha1.ProvisionedObject{
			provObject(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"x"}}`),
		}
	})
	provReconcile(t, r)

	entry := getEntitlement(t, consumer).Status.ProvisionedResources[0]
	if len(entry.Kinds) != 1 || entry.Kinds[0].Kind != "IPClass" {
		t.Fatalf("the ledger forgot what is still installed: %+v", entry.Kinds)
	}

	// And withdrawing the declaration still finds it.
	setConfig(t, root, func(sc *servicesv1alpha1.ServiceConfiguration) { sc.Spec.Provisioning = nil })
	provReconcile(t, r)
	if list := listClasses(t, consumer); len(list.Items) != 0 {
		t.Errorf("teardown left %d objects behind", len(list.Items))
	}
}

// One declaration installing more than the cap is refused whole, never
// truncated: a truncated fan-out looks like a working system.
func TestProvisioningRefusesMoreObjectsThanTheCap(t *testing.T) {
	objects := make([]servicesv1alpha1.ProvisionedObject, 0, maxProvisionedObjectsPerResource+1)
	for i := 0; i <= maxProvisionedObjectsPerResource; i++ {
		objects = append(objects, ipClassRef(fmt.Sprintf("class-%d", i)))
	}
	root := provClient(provConfig(objects...))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	if list := listClasses(t, consumer); len(list.Items) != 0 {
		t.Fatalf("an oversized declaration installed %d objects", len(list.Items))
	}
	if entry := getEntitlement(t, consumer).Status.ProvisionedResources[0]; entry.Reason != reasonTooManyObjects {
		t.Errorf("expected TooManyObjects, got %+v", entry)
	}
}

// The authorization gap must be legible on the object in a running system, not
// only in the source. An object the owning API would authorize against its
// creator reports that the consumer does not hold the grant.
func TestProvisioningReportsUnestablishedAuthorization(t *testing.T) {
	root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6")))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	entry := getEntitlement(t, consumer).Status.ProvisionedResources[0]
	if entry.State != servicesv1alpha1.ProvisionedResourceStateInstalled {
		t.Fatalf("expected the resource to install, got %+v", entry)
	}
	if entry.AuthorizationEstablished {
		t.Error("IPAM authorizes the reference against its creator and nothing here establishes " +
			"the consumer's own grant; authorizationEstablished must be false")
	}
	if !strings.Contains(entry.Message, "platform's authority") {
		t.Errorf("ledger message does not explain whose authority the object rests on: %q", entry.Message)
	}
}

// Withdrawing a declaration removes what it installed; that is the documented
// rollback path.
func TestProvisioningPrunesWhenDeclarationWithdrawn(t *testing.T) {
	root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6")))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)
	if len(listClasses(t, consumer).Items) != 1 {
		t.Fatal("precondition failed: nothing installed")
	}

	setConfig(t, root, func(sc *servicesv1alpha1.ServiceConfiguration) { sc.Spec.Provisioning = nil })
	provReconcile(t, r)

	if list := listClasses(t, consumer); len(list.Items) != 0 {
		t.Errorf("withdrawing the declaration left %d objects behind", len(list.Items))
	}
	cond := apimeta.FindStatusCondition(getEntitlement(t, consumer).Status.Conditions,
		servicesv1alpha1.ConditionTypeProvisioned)
	if cond == nil || cond.Reason != servicesv1alpha1.ReasonNothingToProvision {
		t.Errorf("expected NothingToProvision, got %+v", cond)
	}
}

// The ledger records when provisioning last ran, so a fan-out that has silently
// stopped is visible on the object rather than inferable from someone else's
// incident.
func TestProvisioningRecordsLastEvaluation(t *testing.T) {
	root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6")))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	if getEntitlement(t, consumer).Status.LastProvisioningEvaluation == nil {
		t.Error("lastProvisioningEvaluation was not recorded")
	}
}

// Patching an unchanged verdict would move resourceVersion on every
// five-minute pass, re-triggering every other controller watching
// ServiceEntitlement.
func TestProvisioningSkipsStatusPatchWhenNothingChanged(t *testing.T) {
	root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6")))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	before := getEntitlement(t, consumer)
	if before.Status.LastProvisioningEvaluation == nil {
		t.Fatal("precondition failed: first pass did not record an evaluation")
	}

	provReconcile(t, r)

	after := getEntitlement(t, consumer)
	if after.ResourceVersion != before.ResourceVersion {
		t.Errorf("resourceVersion moved (%s -> %s) though nothing changed", before.ResourceVersion, after.ResourceVersion)
	}
	if !after.Status.LastProvisioningEvaluation.Equal(before.Status.LastProvisioningEvaluation) {
		t.Error("lastProvisioningEvaluation moved though nothing changed and the heartbeat interval had not elapsed")
	}
}

// The heartbeat still has to move eventually, or a long-dead fan-out looks
// identical to one that's idle with nothing to do.
func TestProvisioningRefreshesHeartbeatAfterInterval(t *testing.T) {
	root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6")))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	stale := getEntitlement(t, consumer)
	staleTime := metav1.NewTime(time.Now().Add(-2 * provisioningHeartbeatInterval))
	stale.Status.LastProvisioningEvaluation = &staleTime
	if err := consumer.Status().Update(context.Background(), stale); err != nil {
		t.Fatalf("age the heartbeat: %v", err)
	}

	provReconcile(t, r)

	after := getEntitlement(t, consumer)
	if !after.Status.LastProvisioningEvaluation.After(staleTime.Time) {
		t.Error("lastProvisioningEvaluation did not advance once the heartbeat interval elapsed")
	}
}

// Delivery and access are different facts. A provisioning failure must not
// touch Ready, or it would read as a denial.
func TestProvisioningLeavesReadyConditionAlone(t *testing.T) {
	ent := provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive)
	ent.Status.Conditions = []metav1.Condition{{
		Type:               servicesv1alpha1.ConditionTypeReady,
		Status:             metav1.ConditionTrue,
		Reason:             servicesv1alpha1.ReasonEntitlementActive,
		LastTransitionTime: metav1.Now(),
	}}

	root := provClient(provConfig(provObject(`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"x"}}`)))
	consumer := provConsumerClient(ent)

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	ready := apimeta.FindStatusCondition(getEntitlement(t, consumer).Status.Conditions,
		servicesv1alpha1.ConditionTypeReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Errorf("a provisioning failure changed Ready: %+v", ready)
	}
}

// The active configuration is the most recently created Published one. The two
// existing fan-outs disagree on this rule; this one follows the location
// projection it sits beside.
func TestProvisioningSelectsLatestPublishedConfiguration(t *testing.T) {
	older := provConfig(ipClassRef("tenant-endpoint-ipv6"))
	older.Name = "networking-v1"
	older.CreationTimestamp = metav1.NewTime(metav1.Now().Add(-time.Hour))

	newer := provConfig(ipClassRef("tenant-endpoint-ipv4"))
	newer.Name = "networking-v2"
	newer.CreationTimestamp = metav1.Now()

	root := provClient(older, newer)
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	got := listClasses(t, consumer).Items
	if len(got) != 1 || got[0].GetName() != "tenant-endpoint-ipv4" {
		t.Errorf("expected the newest Published configuration to win, installed: %+v", names(got))
	}
}

func names(items []unstructured.Unstructured) []string {
	out := make([]string, 0, len(items))
	for i := range items {
		out = append(out, items[i].GetName())
	}
	return out
}

// provStatusConsumerClient serves a status subresource for IPClass as well as
// for ServiceEntitlement, so a status write takes the same separate endpoint it
// takes against a real API server.
func provStatusConsumerClient(objs ...client.Object) client.Client {
	ipClass := &unstructured.Unstructured{}
	ipClass.SetGroupVersionKind(ipClassGVK)
	return fake.NewClientBuilder().
		WithScheme(provScheme()).
		WithTypeConverters(fakeTypeConverters()...).
		WithObjects(objs...).
		WithStatusSubresource(&servicesv1alpha1.ServiceEntitlement{}, ipClass).
		Build()
}

// A reference class the provider publishes as usable. Without the declared
// status, a consumer reading the class in their own project cannot tell a class
// that works from one that never came up.
func ipClassRefWithStatus(name, reason string) servicesv1alpha1.ProvisionedObject {
	return provObject(fmt.Sprintf(`{
		"apiVersion": "ipam.miloapis.com/v1alpha1",
		"kind": "IPClass",
		"metadata": {"name": %q},
		"spec": {"source": {"project": %q, "name": %q}},
		"status": {"conditions": [{
			"type": "Ready",
			"status": "True",
			"reason": %q,
			"message": "The class is available in this project.",
			"lastTransitionTime": "2026-01-01T00:00:00Z"
		}]}
	}`, name, provSourceProject, name, reason))
}

func installedCondition(t *testing.T, c client.Client, field string) any {
	t.Helper()
	items := listClasses(t, c).Items
	if len(items) != 1 {
		t.Fatalf("expected one installed class, got %d", len(items))
	}
	conditions, found, err := unstructured.NestedSlice(items[0].Object, "status", "conditions")
	if err != nil || !found || len(conditions) == 0 {
		t.Fatalf("the installed class carries no status.conditions: %+v", items[0].Object["status"])
	}
	return conditions[0].(map[string]any)[field]
}

// The declared status has to reach the status subresource, because an object
// write never carries status past a kind that serves one. Otherwise a consumer
// only ever sees the CRD default.
func TestProvisioningAppliesDeclaredStatusToTheStatusSubresource(t *testing.T) {
	root := provClient(provConfig(ipClassRefWithStatus("tenant-endpoint-ipv6", "Available")))
	consumer := provStatusConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	if got := installedCondition(t, consumer, "reason"); got != "Available" {
		t.Errorf("installed status.conditions[0].reason = %v, want the reason the provider declared", got)
	}

	// The status landed, so the entry is an ordinary Installed with no cause to
	// report.
	entry := getEntitlement(t, consumer).Status.ProvisionedResources[0]
	if entry.State != servicesv1alpha1.ProvisionedResourceStateInstalled || entry.Reason != "" {
		t.Errorf("ledger reports a problem where there is none: %+v", entry)
	}
}

// Editing the status a declaration states is how a provider tells consumers a
// class stopped being usable, so the edit has to converge like any other.
func TestProvisioningConvergesOnDeclaredStatusChange(t *testing.T) {
	root := provClient(provConfig(ipClassRefWithStatus("tenant-endpoint-ipv6", "Available")))
	consumer := provStatusConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	setConfig(t, root, func(sc *servicesv1alpha1.ServiceConfiguration) {
		sc.Spec.Provisioning.Resources[0].Objects = []servicesv1alpha1.ProvisionedObject{
			ipClassRefWithStatus("tenant-endpoint-ipv6", "Withdrawn"),
		}
	})
	provReconcile(t, r)

	if got := installedCondition(t, consumer, "reason"); got != "Withdrawn" {
		t.Errorf("installed status.conditions[0].reason = %v, want the provider's new reason", got)
	}
}

// Every declaration authored before a status could be stated says nothing about
// status. Writing one anyway would take ownership of a field the owning API
// fills in, and overwrite what it put there.
func TestProvisioningLeavesStatusAloneWhenNoneIsDeclared(t *testing.T) {
	root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6")))
	consumer := provStatusConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	// The owning API reports on the object it now holds.
	installed := listClasses(t, consumer).Items[0]
	installed.Object["status"] = map[string]any{"conditions": []any{map[string]any{
		"type":               "Ready",
		"status":             "True",
		"reason":             "ObservedBySourceController",
		"message":            "The class resolved against its source.",
		"lastTransitionTime": "2026-01-01T00:00:00Z",
	}}}
	if err := consumer.Status().Update(context.Background(), &installed); err != nil {
		t.Fatalf("update installed class status: %v", err)
	}

	provReconcile(t, r)

	if got := installedCondition(t, consumer, "reason"); got != "ObservedBySourceController" {
		t.Errorf("a declaration stating no status overwrote the owning API's: %v", got)
	}
}

// A kind that serves no status subresource takes its status in the object write
// itself, so nothing is missing and nothing is reported.
func TestProvisioningInstallsStatusOnAKindWithNoStatusSubresource(t *testing.T) {
	root := provClient(provConfig(ipClassRefWithStatus("tenant-endpoint-ipv6", "Available")))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	if got := installedCondition(t, consumer, "reason"); got != "Available" {
		t.Errorf("installed status.conditions[0].reason = %v, want the declared reason", got)
	}
	entry := getEntitlement(t, consumer).Status.ProvisionedResources[0]
	if entry.State != servicesv1alpha1.ProvisionedResourceStateInstalled || entry.Reason != "" {
		t.Errorf("a kind with no status endpoint was reported as a problem: %+v", entry)
	}
}

// The resync re-evaluates every entitled project every few minutes. A status
// write that is not a no-op costs one write per project per interval and wakes
// every watcher on the installed object. Server-side apply of an unchanged
// value prevents that, so the request body has to be identical each time.
func TestProvisioningStatusApplyIsIdempotentAcrossResyncs(t *testing.T) {
	var applied []string
	consumer := fake.NewClientBuilder().
		WithScheme(provScheme()).
		WithTypeConverters(fakeTypeConverters()...).
		WithObjects(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive)).
		WithStatusSubresource(&servicesv1alpha1.ServiceEntitlement{}, ipClassObject()).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceApply: func(ctx context.Context, c client.Client, subResource string,
				obj runtime.ApplyConfiguration, opts ...client.SubResourceApplyOption) error {
				if isEntitlementApply(obj) {
					return c.Status().Apply(ctx, obj, opts...)
				}
				body, err := json.Marshal(obj)
				if err != nil {
					return err
				}
				applied = append(applied, string(body))
				return c.Status().Apply(ctx, obj, opts...)
			},
		}).
		Build()

	root := provClient(provConfig(ipClassRefWithStatus("tenant-endpoint-ipv6", "Available")))
	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)
	provReconcile(t, r)

	if len(applied) != 2 {
		t.Fatalf("expected one status apply per reconcile, got %d", len(applied))
	}
	if applied[0] != applied[1] {
		t.Errorf("the status apply is not stable across resyncs:\nfirst:  %s\nsecond: %s", applied[0], applied[1])
	}

	// Nothing outside the declared status travels with it. Sending metadata
	// through the status endpoint would claim the object's labels a second time
	// under a different subresource, giving the apply something to churn.
	var body map[string]any
	if err := json.Unmarshal([]byte(applied[0]), &body); err != nil {
		t.Fatalf("unmarshal applied status: %v", err)
	}
	meta, _ := body["metadata"].(map[string]any)
	if len(meta) != 1 || meta["name"] != "tenant-endpoint-ipv6" {
		t.Errorf("the status apply carries metadata beyond the object's name: %+v", meta)
	}
	if _, carried := body["spec"]; carried {
		t.Errorf("the status apply carries the object's spec: %+v", body)
	}
}

// isEntitlementApply tells the entitlement's own status apply apart from the
// installed object's, so an interceptor aimed at the latter lets it through.
func isEntitlementApply(obj runtime.ApplyConfiguration) bool {
	body, err := json.Marshal(obj)
	if err != nil {
		return false
	}
	var typed struct {
		Kind string `json:"kind"`
	}
	return json.Unmarshal(body, &typed) == nil && typed.Kind == "ServiceEntitlement"
}

func ipClassObject() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(ipClassGVK)
	return u
}

// A conflict means a controller in the consumer's plane manages that status
// field. Forcing the apply would make two managers write against each other in
// every entitled project. The object is installed either way, so the ledger
// carries the reason the expected status is missing.
func TestProvisioningReportsAStatusAnotherControllerOwns(t *testing.T) {
	ipClass := ipClassObject()
	consumer := fake.NewClientBuilder().
		WithScheme(provScheme()).
		WithTypeConverters(fakeTypeConverters()...).
		WithObjects(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive)).
		WithStatusSubresource(&servicesv1alpha1.ServiceEntitlement{}, ipClass).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceApply: func(ctx context.Context, c client.Client, subResource string,
				obj runtime.ApplyConfiguration, opts ...client.SubResourceApplyOption) error {
				if isEntitlementApply(obj) {
					return c.Status().Apply(ctx, obj, opts...)
				}
				return apierrors.NewConflict(
					schema.GroupResource{Group: ipClassGVK.Group, Resource: "ipclasses"},
					"tenant-endpoint-ipv6",
					fmt.Errorf("conflict with another field manager"))
			},
		}).
		Build()

	root := provClient(provConfig(ipClassRefWithStatus("tenant-endpoint-ipv6", "Available")))
	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	if len(listClasses(t, consumer).Items) != 1 {
		t.Fatal("the object was not installed")
	}
	entry := getEntitlement(t, consumer).Status.ProvisionedResources[0]
	if entry.State != servicesv1alpha1.ProvisionedResourceStateInstalled {
		t.Errorf("a status that did not land made the resource undelivered: %+v", entry)
	}
	if entry.Reason != reasonStatusNotApplied {
		t.Errorf("ledger reason = %q, want %q", entry.Reason, reasonStatusNotApplied)
	}
	if !strings.Contains(entry.Message, "another controller in this project manages it") {
		t.Errorf("ledger message does not say what stopped the status: %q", entry.Message)
	}
	// Delivery succeeded, so the entitlement still reads Provisioned.
	cond := apimeta.FindStatusCondition(getEntitlement(t, consumer).Status.Conditions,
		servicesv1alpha1.ConditionTypeProvisioned)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("expected Provisioned=True, got %+v", cond)
	}
}

// The ledger message has a schema limit, and one note per object can exceed it.
// The API server refuses an over-long message, which loses the whole ledger
// write.
func TestLedgerMessageIsTruncatedToTheSchemaLimit(t *testing.T) {
	message := truncateMessage(strings.Repeat("a", maxLedgerMessageBytes+100), maxLedgerMessageBytes)
	if len(message) != maxLedgerMessageBytes {
		t.Errorf("truncated message is %d bytes, want %d", len(message), maxLedgerMessageBytes)
	}
	if !strings.HasSuffix(message, "...") {
		t.Errorf("a truncated message does not show it was cut: %q", message[len(message)-8:])
	}
	if got := truncateMessage("short", maxLedgerMessageBytes); got != "short" {
		t.Errorf("a message inside the limit was rewritten: %q", got)
	}
}

// A failed install embeds the API server's error, which has no length bound.
// An over-long ledger message makes the API server refuse the whole status
// write, hiding the failure and retrying it with backoff.
func TestProvisioningTruncatesFailureMessagesToTheSchemaLimit(t *testing.T) {
	longCause := strings.Repeat("admission webhook denied the request; ", 60)
	consumer := fake.NewClientBuilder().
		WithScheme(provScheme()).
		WithTypeConverters(fakeTypeConverters()...).
		WithObjects(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive)).
		WithStatusSubresource(&servicesv1alpha1.ServiceEntitlement{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				// Forbidden passes through to status; an internal error would be
				// replaced by a short generic message and never reach the limit.
				return apierrors.NewForbidden(
					schema.GroupResource{Group: ipClassGVK.Group, Resource: "ipclasses"},
					"tenant-endpoint-ipv6", errors.New(longCause))
			},
		}).
		Build()

	root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6")))
	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	ledger := getEntitlement(t, consumer).Status.ProvisionedResources
	if len(ledger) != 1 {
		t.Fatalf("ledger = %+v, want one entry", ledger)
	}
	if ledger[0].State != servicesv1alpha1.ProvisionedResourceStateFailed {
		t.Errorf("state = %q, want %q", ledger[0].State, servicesv1alpha1.ProvisionedResourceStateFailed)
	}
	if got := len(ledger[0].Message); got > maxLedgerMessageBytes {
		t.Errorf("failure message is %d bytes, above the %d byte schema limit", got, maxLedgerMessageBytes)
	}
	if !strings.HasPrefix(ledger[0].Message, provServiceName) && !strings.Contains(ledger[0].Message, "could not install") {
		t.Errorf("truncation lost the start of the message: %q", ledger[0].Message[:80])
	}
}

func TestLedgerMessageTruncationKeepsCharactersWhole(t *testing.T) {
	message := truncateMessage(strings.Repeat("é", maxLedgerMessageBytes), maxLedgerMessageBytes)
	if len(message) > maxLedgerMessageBytes {
		t.Errorf("truncated message is %d bytes, above %d", len(message), maxLedgerMessageBytes)
	}
	if !utf8.ValidString(message) {
		t.Errorf("truncation split a multi-byte character")
	}
}

// existingClass is an IPClass already in the consumer project before
// provisioning reaches it, created by whoever the metadata says.
func existingClass(name string, mutate func(*unstructured.Unstructured)) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(ipClassGVK)
	u.SetName(name)
	_ = unstructured.SetNestedMap(u.Object, map[string]any{"policy": "customer-owned"}, "spec")
	if mutate != nil {
		mutate(u)
	}
	return u
}

func getClass(t *testing.T, c client.Client, name string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(ipClassGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, u); err != nil {
		t.Fatalf("get class %q: %v", name, err)
	}
	return u
}

func assertUntouched(t *testing.T, got *unstructured.Unstructured) {
	t.Helper()
	if ownedBy(got.GetOwnerReferences(), provEntitlementUID) {
		t.Errorf("the entitlement took ownership of an object it did not install: %+v", got.GetOwnerReferences())
	}
	if got.GetLabels()[labelEntitlementName] == provEntitlement {
		t.Errorf("an object the entitlement did not install was labelled as its own: %+v", got.GetLabels())
	}
	if policy, _, _ := unstructured.NestedString(got.Object, "spec", "policy"); policy != "customer-owned" {
		t.Errorf("an object the entitlement did not install was rewritten: %+v", got.Object["spec"])
	}
	if _, found, _ := unstructured.NestedMap(got.Object, "spec", "source"); found {
		t.Errorf("an object the entitlement did not install received the declared spec: %+v", got.Object["spec"])
	}
}

// A name the customer already uses is theirs. Enabling a service must not
// rewrite that object, and disabling it must not delete it.
func TestProvisioningLeavesAnExistingUnmanagedObjectUnchanged(t *testing.T) {
	root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6"), ipClassRef("tenant-vpc-ipv6")))
	consumer := provConsumerClient(
		provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive),
		existingClass("tenant-endpoint-ipv6", nil),
	)

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	assertUntouched(t, getClass(t, consumer, "tenant-endpoint-ipv6"))
	// The declaration's other objects still install.
	if !ownedBy(getClass(t, consumer, "tenant-vpc-ipv6").GetOwnerReferences(), provEntitlementUID) {
		t.Errorf("the declaration's free name was not installed")
	}

	ent := getEntitlement(t, consumer)
	entry := ent.Status.ProvisionedResources[0]
	if entry.State != servicesv1alpha1.ProvisionedResourceStateUnprovisionable ||
		entry.Reason != reasonObjectExists || entry.ObjectCount != 1 {
		t.Fatalf("expected Unprovisionable/ObjectExists with one object installed, got %+v", entry)
	}
	want := `IPClass "tenant-endpoint-ipv6" already exists in this project and was not installed by the platform, so it was left unchanged.`
	if entry.Message != want {
		t.Errorf("ledger message = %q, want %q", entry.Message, want)
	}
	cond := apimeta.FindStatusCondition(ent.Status.Conditions, servicesv1alpha1.ConditionTypeProvisioned)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != servicesv1alpha1.ReasonNotProvisioned {
		t.Errorf("expected Provisioned=False/NotProvisioned, got %+v", cond)
	}

	// Disabling the service removes what it installed and nothing else.
	ent.Status.Phase = servicesv1alpha1.EntitlementPhaseRejected
	if err := consumer.Status().Update(context.Background(), ent); err != nil {
		t.Fatalf("update entitlement phase: %v", err)
	}
	provReconcile(t, r)
	list := listClasses(t, consumer)
	if len(list.Items) != 1 || list.Items[0].GetName() != "tenant-endpoint-ipv6" {
		t.Fatalf("teardown should remove only the installed class, left %d", len(list.Items))
	}
	assertUntouched(t, &list.Items[0])
}

// Two services that declare the same name do not trade the object between
// them on every resync: the first to install it keeps it.
func TestProvisioningDoesNotTakeOverAnotherEntitlementsObject(t *testing.T) {
	root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6")))
	consumer := provConsumerClient(
		provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive),
		existingClass("tenant-endpoint-ipv6", func(u *unstructured.Unstructured) {
			u.SetLabels(map[string]string{
				labelManagedBy:           labelManagedByValue,
				labelEntitlementName:     "compute",
				labelProvisionedResource: "address-classes",
			})
			isController := true
			u.SetOwnerReferences([]metav1.OwnerReference{{
				APIVersion: servicesv1alpha1.GroupVersion.String(),
				Kind:       "ServiceEntitlement",
				Name:       "compute",
				UID:        "ent-compute-uid",
				Controller: &isController,
			}})
		}),
	)

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	got := getClass(t, consumer, "tenant-endpoint-ipv6")
	assertUntouched(t, got)
	if !ownedBy(got.GetOwnerReferences(), "ent-compute-uid") || got.GetLabels()[labelEntitlementName] != "compute" {
		t.Errorf("the other entitlement lost its object: %+v %+v", got.GetOwnerReferences(), got.GetLabels())
	}
	entry := getEntitlement(t, consumer).Status.ProvisionedResources[0]
	if entry.Reason != reasonObjectExists || !strings.Contains(entry.Message, "for another service") {
		t.Errorf("expected ObjectExists naming another service, got %+v", entry)
	}
}

// What this entitlement installed is still its to update.
func TestProvisioningUpdatesAnObjectThisEntitlementInstalled(t *testing.T) {
	root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6")))
	consumer := provConsumerClient(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	setConfig(t, root, func(sc *servicesv1alpha1.ServiceConfiguration) {
		sc.Spec.Provisioning.Resources[0].Objects = []servicesv1alpha1.ProvisionedObject{provObject(`{
			"apiVersion": "ipam.miloapis.com/v1alpha1",
			"kind": "IPClass",
			"metadata": {"name": "tenant-endpoint-ipv6"},
			"spec": {"source": {"project": "moved", "name": "tenant-endpoint-ipv6"}}
		}`)}
	})
	provReconcile(t, r)

	source, _, _ := unstructured.NestedMap(getClass(t, consumer, "tenant-endpoint-ipv6").Object, "spec", "source")
	if source["project"] != "moved" {
		t.Errorf("an object the entitlement installed was not updated: %+v", source)
	}
	entry := getEntitlement(t, consumer).Status.ProvisionedResources[0]
	if entry.State != servicesv1alpha1.ProvisionedResourceStateInstalled || entry.Reason != "" {
		t.Errorf("expected a clean install, got %+v", entry)
	}
}

// An object older than its entitlement was someone else's before an earlier
// version of the controller took it over. It is reported, not released.
func TestProvisioningReportsAnObjectTakenOverInThePast(t *testing.T) {
	enabledAt := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	ent := provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive)
	ent.CreationTimestamp = enabledAt

	root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6")))
	consumer := provConsumerClient(ent, existingClass("tenant-endpoint-ipv6", func(u *unstructured.Unstructured) {
		u.SetCreationTimestamp(metav1.NewTime(enabledAt.Add(-24 * time.Hour)))
		u.SetLabels(map[string]string{
			labelManagedBy:           labelManagedByValue,
			labelEntitlementName:     provEntitlement,
			labelProvisionedResource: "address-classes",
		})
		isController := true
		u.SetOwnerReferences([]metav1.OwnerReference{{
			APIVersion: servicesv1alpha1.GroupVersion.String(),
			Kind:       "ServiceEntitlement",
			Name:       provEntitlement,
			UID:        provEntitlementUID,
			Controller: &isController,
		}})
	}))

	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	if !ownedBy(getClass(t, consumer, "tenant-endpoint-ipv6").GetOwnerReferences(), provEntitlementUID) {
		t.Errorf("ownership of a previously adopted object changed silently")
	}
	entry := getEntitlement(t, consumer).Status.ProvisionedResources[0]
	if entry.State != servicesv1alpha1.ProvisionedResourceStateInstalled || entry.Reason != reasonObjectAdopted ||
		!strings.Contains(entry.Message, "disabling the service will delete it") {
		t.Errorf("expected Installed/ObjectAdopted with a warning, got %+v", entry)
	}
}

// A declaration that fails to install right after enablement is usually held
// up by something that clears within seconds, such as the owning service's
// webhook not yet serving the new project. Waiting for the five-minute resync
// left setup stalled long after the cause was gone.
func TestProvisioningRetriesFailedResourcesSoonAfterTheCauseClears(t *testing.T) {
	failing := true
	consumer := fake.NewClientBuilder().
		WithScheme(provScheme()).
		WithTypeConverters(fakeTypeConverters()...).
		WithObjects(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive)).
		WithStatusSubresource(&servicesv1alpha1.ServiceEntitlement{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				if failing {
					return apierrors.NewForbidden(
						schema.GroupResource{Group: ipClassGVK.Group, Resource: "ipclasses"},
						"tenant-endpoint-ipv6", errors.New("consumer project not engaged yet"))
				}
				return c.Apply(ctx, obj, opts...)
			},
		}).
		Build()

	root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6")))
	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	clk := testingclock.NewFakeClock(time.Now())
	r.clock = clk

	// Each step advances the clock by elapse, then runs one pass. The retry
	// delay is a tenth of how long the entitlement has been failing, never
	// under a second. A doubling backoff from 2s retried at 2, 6, 14, 30, 62
	// and 126s, so a cause that cleared at 70s held setup until 126s.
	steps := []struct {
		elapse  time.Duration
		failing bool
		want    time.Duration
	}{
		{failing: true, want: provisioningFailureRetryMin},
		{elapse: 5 * time.Second, failing: true, want: provisioningFailureRetryMin},
		{elapse: 25 * time.Second, failing: true, want: 3 * time.Second},
		{elapse: 40 * time.Second, failing: true, want: 7 * time.Second},
		{elapse: 7 * time.Second, failing: false, want: provisioningResyncInterval},
		// After a success, a new failure starts again from the short delay.
		{elapse: time.Minute, failing: true, want: provisioningFailureRetryMin},
	}
	for i, step := range steps {
		clk.Step(step.elapse)
		failing = step.failing
		if got := provReconcile(t, r).RequeueAfter; got != step.want {
			t.Errorf("pass %d (failing=%v): requeueAfter = %s, want %s", i+1, step.failing, got, step.want)
		}
	}
}

func TestProvisioningFailureRetryIsCappedAtTheResyncInterval(t *testing.T) {
	clk := testingclock.NewFakeClock(time.Now())
	r := &ProvisioningReconciler{clock: clk}
	r.failureRetryDelay("entitlement-uid")
	clk.Step(provisioningFailureRetryFraction * provisioningResyncInterval * 2)
	if got := r.failureRetryDelay("entitlement-uid"); got != provisioningResyncInterval {
		t.Errorf("retry delay after a long run of failures = %s, want the %s resync cap", got, provisioningResyncInterval)
	}
}

// Entitlements deleted while failing never clear their own entry, so clearing
// any entitlement drops entries that are far older than a retry cycle.
func TestProvisioningFailureRetryForgetsStaleEntitlements(t *testing.T) {
	clk := testingclock.NewFakeClock(time.Now())
	r := &ProvisioningReconciler{clock: clk}
	r.failureRetryDelay("deleted-uid")
	r.failureRetryDelay("recent-uid")
	clk.Step(25 * time.Hour)
	r.failureRetryDelay("new-uid")
	r.clearFailureRetry("other-uid")
	if _, ok := r.failingSince["deleted-uid"]; ok {
		t.Errorf("a failure entry older than a day was kept")
	}
	if _, ok := r.failingSince["new-uid"]; !ok {
		t.Errorf("a current failure entry was dropped")
	}
}

// Disabling a service while a pass is installing used to leave the object
// behind. Teardown swept the project before the install landed and released
// the entitlement, and the object then pointed at an owner that no longer
// existed, which garbage collection in a project control plane did not reclaim.
func TestProvisioningRemovesWhatItInstalledWhenTheEntitlementIsDeletedMidPass(t *testing.T) {
	var once sync.Once
	consumer := fake.NewClientBuilder().
		WithScheme(provScheme()).
		WithTypeConverters(fakeTypeConverters()...).
		WithObjects(provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive)).
		WithStatusSubresource(&servicesv1alpha1.ServiceEntitlement{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				if err := c.Apply(ctx, obj, opts...); err != nil {
					return err
				}
				// The customer disables the service, and teardown completes,
				// between this pass reading the entitlement and finishing.
				var err error
				once.Do(func() {
					err = c.Delete(ctx, provEntitlementObj(servicesv1alpha1.EntitlementPhaseActive))
				})
				return err
			},
		}).
		Build()

	root := provClient(provConfig(ipClassRef("tenant-endpoint-ipv6")))
	r := newProvReconciler(root, map[string]client.Client{provConsumerProject: consumer})
	provReconcile(t, r)

	if got := listClasses(t, consumer).Items; len(got) != 0 {
		t.Errorf("objects outlived their entitlement: %+v", names(got))
	}
}
