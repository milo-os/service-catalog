// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// withheldErrorReason stands in, on customer-visible status, for an error the
// customer must not read.
const withheldErrorReason = "an internal platform error occurred and it will be retried"

// customerErrorDetail returns err's text when a project member may read it, and
// false when it has to stay in the operator log.
//
// ServiceEntitlement status is readable by every member of the project, and
// the APIs this operator writes to are not careful about what an internal
// error carries: a server error can hold a database address, a user name, or a
// stack trace. So the test is an allowlist. Only reasons that describe the
// request itself, not the machinery that served it, pass: the object was
// invalid, refused, missing, or contended, or its kind is not served. Every
// other error, including one that is not an API error at all, is withheld.
func customerErrorDetail(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	if apimeta.IsNoMatchError(err) {
		return err.Error(), true
	}
	switch apierrors.ReasonForError(err) {
	case metav1.StatusReasonInvalid,
		metav1.StatusReasonForbidden,
		metav1.StatusReasonNotFound,
		metav1.StatusReasonConflict,
		metav1.StatusReasonAlreadyExists:
		return err.Error(), true
	}
	return "", false
}

// customerErrorReason is what customer-visible status says about err. An error
// that is withheld is logged in full, with keysAndValues for context, so the
// operator keeps what the customer does not see.
func customerErrorReason(ctx context.Context, err error, keysAndValues ...any) string {
	if detail, ok := customerErrorDetail(err); ok {
		return detail
	}
	log.FromContext(ctx).Error(err, "withheld an internal error from customer-visible status", keysAndValues...)
	return withheldErrorReason
}
