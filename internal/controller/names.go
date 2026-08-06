// SPDX-License-Identifier: AGPL-3.0-only

package controller

import "strings"

// encodeName produces a DNS-safe name from a canonical identifier by
// lowercasing and replacing "/" and "." with "-".
func encodeName(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, ".", "-")
	return s
}

// encodeServicePricingName produces the DNS-safe ServicePricing metadata.name
// from a metric or charge identifier. Unlike encodeName, the first "/" becomes
// "--" so the service group remains visually distinct from the rest of the
// path:
//
//	compute.datumapis.com/instance/cpu-allocated
//	  → compute-datumapis-com--instance-cpu-allocated
func encodeServicePricingName(metricOrChargeName string) string {
	s := strings.ToLower(metricOrChargeName)
	idx := strings.IndexByte(s, '/')
	if idx < 0 {
		return strings.ReplaceAll(s, ".", "-")
	}
	host := strings.ReplaceAll(s[:idx], ".", "-")
	rest := strings.ReplaceAll(s[idx+1:], "/", "-")
	return host + "--" + rest
}
