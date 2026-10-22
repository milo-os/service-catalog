// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	resourcemanagerv1alpha1 "go.miloapis.com/milo/pkg/apis/resourcemanager/v1alpha1"
)

func TestGetConsumerProjectConfirmsCacheMissWithAPIReader(t *testing.T) {
	project := &resourcemanagerv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: testConsumerProject}}
	cacheClient := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	apiReader := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(project).Build()

	got, err := getConsumerProject(context.Background(), cacheClient, apiReader, testConsumerProject)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Name != testConsumerProject {
		t.Fatalf("Project = %#v, want live APIReader object after cache miss", got)
	}
}
