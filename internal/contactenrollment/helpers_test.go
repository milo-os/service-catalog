// SPDX-License-Identifier: AGPL-3.0-only

package contactenrollment

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	notificationv1alpha1 "go.miloapis.com/milo/pkg/apis/notification/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := notificationv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add notification scheme: %v", err)
	}
	if err := servicesv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add services scheme: %v", err)
	}
	return s
}

// newFakeClient builds a fake client with the field indexes this package's
// functions depend on, registered exactly the way
// ContactEnrollmentReconciler.SetupWithManager must register them on a real
// manager cache.
func newFakeClient(t *testing.T, objs ...client.Object) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithIndex(&notificationv1alpha1.Contact{}, ContactSubjectNameIndex, func(obj client.Object) []string {
			c := obj.(*notificationv1alpha1.Contact)
			if c.Spec.SubjectRef == nil || c.Spec.SubjectRef.Name == "" {
				return nil
			}
			return []string{c.Spec.SubjectRef.Name}
		}).
		WithIndex(&notificationv1alpha1.Contact{}, ContactEmailIndex, func(obj client.Object) []string {
			c := obj.(*notificationv1alpha1.Contact)
			if c.Spec.Email == "" {
				return nil
			}
			return []string{c.Spec.Email}
		}).
		WithIndex(&notificationv1alpha1.ContactGroupMembershipRemoval{}, MembershipRemovalContactRefNameIndex, func(obj client.Object) []string {
			r := obj.(*notificationv1alpha1.ContactGroupMembershipRemoval)
			if r.Spec.ContactRef.Name == "" {
				return nil
			}
			return []string{r.Spec.ContactRef.Name}
		}).
		WithObjects(objs...).
		Build()
}
