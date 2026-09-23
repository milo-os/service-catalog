// SPDX-License-Identifier: AGPL-3.0-only

package contactenrollment

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	notificationv1alpha1 "go.miloapis.com/milo/pkg/apis/notification/v1alpha1"
	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

const testNamespace = "milo-system"

func newContact(name, subjectName, email string) *notificationv1alpha1.Contact {
	c := &notificationv1alpha1.Contact{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       notificationv1alpha1.ContactSpec{Email: email},
	}
	if subjectName != "" {
		c.Spec.SubjectRef = &notificationv1alpha1.SubjectReference{
			APIGroup: "iam.miloapis.com",
			Kind:     "User",
			Name:     subjectName,
		}
	}
	return c
}

func newRequester(name, email string) *servicesv1alpha1.RequesterRef {
	return &servicesv1alpha1.RequesterRef{
		APIGroup: "iam.miloapis.com",
		Kind:     "User",
		Name:     name,
		Email:    email,
	}
}

func TestResolveContact_NilRequester(t *testing.T) {
	c := newFakeClient(t)
	_, err := ResolveContact(context.Background(), c, testNamespace, nil)
	if !errors.Is(err, ErrRequesterUnknown) {
		t.Fatalf("ResolveContact() error = %v, want ErrRequesterUnknown", err)
	}
}

func TestResolveContact_MatchesBySubjectName(t *testing.T) {
	contact := newContact("user-abc123", "user-abc123", "alice@example.com")
	c := newFakeClient(t, contact)

	got, err := ResolveContact(context.Background(), c, testNamespace, newRequester("user-abc123", "alice@example.com"))
	if err != nil {
		t.Fatalf("ResolveContact() error = %v", err)
	}
	if got.Name != contact.Name {
		t.Errorf("ResolveContact() = %q, want %q", got.Name, contact.Name)
	}
}

func TestResolveContact_IgnoresNonUserSubject(t *testing.T) {
	// A Contact whose SubjectRef.Name matches but whose Kind isn't User
	// (e.g. an Organization contact) must not be picked up as the
	// requester's own contact.
	contact := &notificationv1alpha1.Contact{
		ObjectMeta: metav1.ObjectMeta{Name: "org-contact", Namespace: testNamespace},
		Spec: notificationv1alpha1.ContactSpec{
			Email: "org@example.com",
			SubjectRef: &notificationv1alpha1.SubjectReference{
				APIGroup: "resourcemanager.miloapis.com",
				Kind:     "Organization",
				Name:     "user-abc123",
			},
		},
	}
	c := newFakeClient(t, contact)

	_, err := ResolveContact(context.Background(), c, testNamespace, newRequester("user-abc123", ""))
	if !errors.Is(err, ErrContactNotFound) {
		t.Fatalf("ResolveContact() error = %v, want ErrContactNotFound", err)
	}
}

func TestResolveContact_FallsBackToEmail(t *testing.T) {
	// No Contact references this User yet (the requester registered before
	// their Contact was linked), but one exists with a matching email.
	contact := newContact("legacy-contact", "", "alice@example.com")
	c := newFakeClient(t, contact)

	got, err := ResolveContact(context.Background(), c, testNamespace, newRequester("user-abc123", "alice@example.com"))
	if err != nil {
		t.Fatalf("ResolveContact() error = %v", err)
	}
	if got.Name != contact.Name {
		t.Errorf("ResolveContact() = %q, want %q", got.Name, contact.Name)
	}
}

func TestResolveContact_SubjectMatchWinsOverEmail(t *testing.T) {
	bySubject := newContact("subject-contact", "user-abc123", "old@example.com")
	byEmail := newContact("email-contact", "someone-else", "alice@example.com")
	c := newFakeClient(t, bySubject, byEmail)

	got, err := ResolveContact(context.Background(), c, testNamespace, newRequester("user-abc123", "alice@example.com"))
	if err != nil {
		t.Fatalf("ResolveContact() error = %v", err)
	}
	if got.Name != bySubject.Name {
		t.Errorf("ResolveContact() = %q, want the subject-matched contact %q", got.Name, bySubject.Name)
	}
}

func TestResolveContact_NotFound(t *testing.T) {
	c := newFakeClient(t)
	_, err := ResolveContact(context.Background(), c, testNamespace, newRequester("user-abc123", "alice@example.com"))
	if !errors.Is(err, ErrContactNotFound) {
		t.Fatalf("ResolveContact() error = %v, want ErrContactNotFound", err)
	}
}

func TestResolveContact_NoNameNoEmail(t *testing.T) {
	c := newFakeClient(t)
	_, err := ResolveContact(context.Background(), c, testNamespace, &servicesv1alpha1.RequesterRef{})
	if !errors.Is(err, ErrContactNotFound) {
		t.Fatalf("ResolveContact() error = %v, want ErrContactNotFound", err)
	}
}
