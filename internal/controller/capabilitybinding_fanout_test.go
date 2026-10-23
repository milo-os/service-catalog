// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"sort"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	resourcemanagerv1alpha1 "go.miloapis.com/milo/pkg/apis/resourcemanager/v1alpha1"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const (
	cbOtherProject     = "dns-proj"
	cbOtherService     = "dns-miloapis-com"
	cbOtherServiceName = "dns.miloapis.com"
	cbOtherAgent       = "dns-diagnostics-agent"
)

func cbFanoutEntitlement(name, serviceRef, canonical string) *servicesv1alpha1.ServiceEntitlement {
	return &servicesv1alpha1.ServiceEntitlement{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       servicesv1alpha1.ServiceEntitlementSpec{ServiceRef: servicesv1alpha1.ServiceRef{Name: serviceRef}},
		Status: servicesv1alpha1.ServiceEntitlementStatus{
			Phase:       servicesv1alpha1.EntitlementPhaseActive,
			ServiceName: canonical,
		},
	}
}

// newCapabilityFanoutReconciler builds a catalog with two services, each with
// an agent, and three projects: one entitled to compute (twice over, so the
// fan-out has something to collapse), one entitled to dns, and one with
// nothing.
func newCapabilityFanoutReconciler() *CapabilityBindingReconciler {
	otherService := cbServiceObject()
	otherService.Name = cbOtherService
	otherService.Spec.ServiceName = cbOtherServiceName
	otherAgent := cbAgentObject(servicesv1alpha1.PhasePublished)
	otherAgent.Name = cbOtherAgent
	otherAgent.Spec.ServiceRef.Name = cbOtherService

	root := cbRootClient(
		cbServiceObject(), otherService,
		cbAgentObject(servicesv1alpha1.PhasePublished), otherAgent,
		&resourcemanagerv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: cbOtherProject}},
		&resourcemanagerv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "idle-proj"}},
	)
	mgr := newTestManager()
	mgr.add(cbProject, newFakeClient(
		cbFanoutEntitlement(cbEntitlement, cbService, cbServiceName),
		cbFanoutEntitlement("compute-dependency", cbService, cbServiceName),
	))
	mgr.add(cbOtherProject, newFakeClient(cbFanoutEntitlement("dns", cbOtherService, cbOtherServiceName)))
	mgr.add("idle-proj", newFakeClient())
	return &CapabilityBindingReconciler{rootClient: root, rootAPIReader: root, Manager: mgr, Scheme: capabilityScheme()}
}

func requestedProjects(t *testing.T, reqs []mcreconcile.Request) []string {
	t.Helper()
	var projects []string
	for _, req := range reqs {
		if req.Name != locationProjectionRequestName {
			t.Errorf("request for %s names %q, want the project key %q", req.ClusterName, req.Name, locationProjectionRequestName)
		}
		projects = append(projects, string(req.ClusterName))
	}
	sort.Strings(projects)
	return projects
}

func assertProjects(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s enqueued %v, want %v", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s enqueued %v, want %v", what, got, want)
		}
	}
}

// A catalog change reaches each entitled project once, and no other project.
func TestCapabilityFanoutFromService(t *testing.T) {
	r := newCapabilityFanoutReconciler()
	got := requestedProjects(t, enqueueProjectsForServiceName(context.Background(), r.rootClient, r.Manager, cbServiceName))
	assertProjects(t, "a compute Service change", got, cbProject)
}

// An agent change reaches the projects entitled to the agent's service.
func TestCapabilityFanoutFromAgent(t *testing.T) {
	r := newCapabilityFanoutReconciler()
	got := requestedProjects(t, r.enqueueProjectsForServiceRef(context.Background(), cbService))
	assertProjects(t, "a compute agent change", got, cbProject)
}

// A configuration moved to another agent reaches the projects of both: the
// old agent's projects lose a binding, the new agent's gain one.
func TestCapabilityFanoutFromAgentConfigurationMove(t *testing.T) {
	r := newCapabilityFanoutReconciler()
	before := cbConfigObject(cbConfigV1, "v1", servicesv1alpha1.PhasePublished)
	after := before.DeepCopy()
	after.Spec.ServiceAgentRef.Name = cbOtherAgent

	got := requestedProjects(t, r.agentConfigurationRequests(context.Background(), before, after))
	assertProjects(t, "moving a configuration between agents", got, cbProject, cbOtherProject)

	got = requestedProjects(t, r.agentConfigurationRequests(context.Background(), before, before))
	assertProjects(t, "an edit that keeps the agent", got, cbProject)
}
