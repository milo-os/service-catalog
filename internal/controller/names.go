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
