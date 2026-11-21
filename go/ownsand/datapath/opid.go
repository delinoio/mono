// SPDX-License-Identifier: Apache-2.0

package datapath

import (
	"time"

	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Admission window of caller-generated operation IDs (exec_id, read_id and
// write_id), checked against the agent's host clock when the first frame of
// an unknown ID arrives.
const (
	// OperationIDMaxAge: an unknown ID must be less than this old.
	OperationIDMaxAge = 15 * time.Minute
	// OperationIDMaxFuture: an unknown ID must not be more than this far in
	// the future.
	OperationIDMaxFuture = 5 * time.Minute
	// EndedOperationMemory is how long the agent remembers the ID of an
	// ended or expired operation, on its monotonic clock.
	EndedOperationMemory = 30 * time.Minute
)

// AdmissionReason says why an operation ID was not admitted.
type AdmissionReason int

const (
	AdmissionReasonUnspecified AdmissionReason = iota
	// AdmissionReasonInvalid: the ID is not a canonical UUID v7.
	AdmissionReasonInvalid
	// AdmissionReasonExpired: the ID is 15 minutes old or older.
	AdmissionReasonExpired
	// AdmissionReasonFuture: the ID is more than 5 minutes in the future.
	AdmissionReasonFuture
)

func (r AdmissionReason) String() string {
	switch r {
	case AdmissionReasonInvalid:
		return "invalid"
	case AdmissionReasonExpired:
		return "expired"
	case AdmissionReasonFuture:
		return "future"
	}
	return "unspecified"
}

// AdmissionError reports an operation ID outside the admission window.
// Code is INVALID_ARGUMENT, NOT_FOUND (expired, like a remembered expired
// ID) or FAILED_PRECONDITION (clock skew).
type AdmissionError struct {
	Code   ownsandv1.ErrorCode
	Reason AdmissionReason
}

func (e *AdmissionError) Error() string {
	switch e.Reason {
	case AdmissionReasonExpired:
		return "operation expired, or the caller's clock may be behind"
	case AdmissionReasonFuture:
		return "operation ID is more than 5 minutes in the future; the caller's clock may be ahead"
	}
	return "operation ID is not a canonical UUID v7"
}

// CheckOperationID checks an unknown operation ID against the admission
// window at now (the agent's host clock). It returns nil when the ID's
// timestamp is less than 15 minutes old and not more than 5 minutes in the
// future, and *AdmissionError otherwise.
func CheckOperationID(id string, now time.Time) error {
	parsed, err := uuidv7.Parse(id)
	if err != nil {
		return &AdmissionError{Code: ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, Reason: AdmissionReasonInvalid}
	}
	age := now.Sub(parsed.Time())
	switch {
	case age >= OperationIDMaxAge:
		return &AdmissionError{Code: ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, Reason: AdmissionReasonExpired}
	// Compare age directly: negating a saturated time.Duration (MinInt64 for a
	// far-future ID) overflows back to MinInt64 and would skip this check.
	case age < -OperationIDMaxFuture:
		return &AdmissionError{Code: ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, Reason: AdmissionReasonFuture}
	}
	return nil
}

// Detail returns the ErrorDetail of the error frame that rejects the
// operation.
func (e *AdmissionError) Detail() *ownsandv1.ErrorDetail {
	return &ownsandv1.ErrorDetail{Code: e.Code, Message: e.Error()}
}
