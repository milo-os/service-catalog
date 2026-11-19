// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	servicesv1alpha1 "go.miloapis.com/service-catalog/api/v1alpha1"
)

func TestServiceActivationRequestValidateCreateAuthorizationDecision(t *testing.T) {
	tests := []struct {
		name       string
		authorized string
		reason     string
		wantError  string
	}{
		{name: "authorized", authorized: servicesv1alpha1.ServiceActivationAuthorizedValueTrue, reason: "ConsumerPolicyAllowed"},
		{name: "denied", authorized: servicesv1alpha1.ServiceActivationAuthorizedValueFalse, reason: "ConsumerNotOptedIn"},
		{name: "missing decision", reason: "ConsumerPolicyAllowed", wantError: "Required value"},
		{name: "invalid decision", authorized: "forged", reason: "ConsumerPolicyAllowed", wantError: "supported values"},
		{name: "missing reason", authorized: servicesv1alpha1.ServiceActivationAuthorizedValueTrue, wantError: "Required value"},
	}

	webhook := &serviceActivationRequestWebhook{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := &servicesv1alpha1.ServiceActivationRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name: "activate-service",
					Annotations: map[string]string{
						servicesv1alpha1.ServiceActivationAuthorizedAnnotation:          tt.authorized,
						servicesv1alpha1.ServiceActivationAuthorizationReasonAnnotation: tt.reason,
					},
				},
				Spec: servicesv1alpha1.ServiceActivationRequestSpec{
					RequestedBy: servicesv1alpha1.ActorReference{Username: "provider@example.com"},
				},
			}

			_, err := webhook.ValidateCreate(context.Background(), request)
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("ValidateCreate() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("ValidateCreate() error = %v, want error containing %q", err, tt.wantError)
			}
		})
	}
}
