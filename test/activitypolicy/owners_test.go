// SPDX-License-Identifier: AGPL-3.0-only

package activitypolicy_test

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

// The owner the activity processor reads from a policy's labels to route the
// alerts for a failing policy. A policy without them falls back to the
// activity processor's own owner. The platform's service catalog lists this
// repository under the control-plane service, team platform.
var policyOwner = map[string]string{
	"meta.datumapis.com/service": "control-plane",
	"meta.datumapis.com/team":    "platform",
}

type policiesKustomization struct {
	Resources []string `json:"resources"`
	Labels    []struct {
		Pairs map[string]string `json:"pairs"`
	} `json:"labels"`
}

func readPoliciesKustomization(t *testing.T) policiesKustomization {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.Dir(policiesGlob), "kustomization.yaml"))
	if err != nil {
		t.Fatalf("read the policies' kustomization: %v", err)
	}
	var k policiesKustomization
	if err := yaml.Unmarshal(b, &k); err != nil {
		t.Fatalf("parse the policies' kustomization: %v", err)
	}
	return k
}

// declaredOwner returns the labels the kustomization sets on its policies, by
// label name.
func declaredOwner(k policiesKustomization) map[string]string {
	owner := map[string]string{}
	for _, l := range k.Labels {
		for name, value := range l.Pairs {
			owner[name] = value
		}
	}
	return owner
}

// unlistedPolicyFiles returns the policy files in the folder that the
// kustomization does not list in its resources.
func unlistedPolicyFiles(t *testing.T, k policiesKustomization) []string {
	t.Helper()
	files, err := filepath.Glob(policiesGlob)
	if err != nil {
		t.Fatalf("glob %q: %v", policiesGlob, err)
	}
	if len(files) == 0 {
		t.Fatalf("no policy files matched %q", policiesGlob)
	}
	listed := map[string]bool{}
	for _, r := range k.Resources {
		listed[r] = true
	}
	var unlisted []string
	for _, f := range files {
		if name := filepath.Base(f); !listed[name] {
			unlisted = append(unlisted, name)
		}
	}
	return unlisted
}

// TestEveryPolicyDeclaresItsOwner fails when the policies' kustomization sets
// no owner or another owner on its policies, or when a policy file sits
// outside its resources, where the kustomization's owner labels would not
// reach it.
func TestEveryPolicyDeclaresItsOwner(t *testing.T) {
	k := readPoliciesKustomization(t)
	owner := declaredOwner(k)
	for label, want := range policyOwner {
		if got := owner[label]; got != want {
			t.Errorf("the policies' kustomization sets %s to %q on its policies, want %q", label, got, want)
		}
	}
	for _, name := range unlistedPolicyFiles(t, k) {
		t.Errorf("%s: not in the policies' resources, so the owner labels never reach it", name)
	}
}
