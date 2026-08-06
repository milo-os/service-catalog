// SPDX-License-Identifier: AGPL-3.0-only

package controller

import "testing"

func TestEncodeServicePricingName(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{
			in:   "compute.datumapis.com/instance/cpu-allocated",
			want: "compute-datumapis-com--instance-cpu-allocated",
		},
		{
			in:   "Compute.Datumapis.Com/Instance/CPU-Allocated",
			want: "compute-datumapis-com--instance-cpu-allocated",
		},
		{
			in:   "networking.datumapis.com/transfer/egress-internet",
			want: "networking-datumapis-com--transfer-egress-internet",
		},
		{
			in:   "assistant.miloapis.com/conversation/input-tokens",
			want: "assistant-miloapis-com--conversation-input-tokens",
		},
		{
			// Single path segment after the group.
			in:   "compute.datumapis.com/platform-fee",
			want: "compute-datumapis-com--platform-fee",
		},
		{
			// No slash: only dots are rewritten.
			in:   "platform-fee",
			want: "platform-fee",
		},
		{
			in:   "simple.name",
			want: "simple-name",
		},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := encodeServicePricingName(tt.in); got != tt.want {
				t.Errorf("encodeServicePricingName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
