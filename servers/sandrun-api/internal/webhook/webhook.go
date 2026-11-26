// SPDX-License-Identifier: FSL-1.1-ALv2

// Package webhook holds what every webhook endpoint of sandrun-api shares: the
// delivery ID store (the webhook_deliveries table, one row per sender and
// delivery ID, kept for 7 days), the 1 MiB body limit and the delivery
// metric. The owning contract is the Webhooks section of
// docs/servers-sandrun-api-foundation.md.
//
// A webhook layer mounts its endpoint with app.Registry.Handle (for
// example "POST /webhooks/github"). The handler reads the body with
// ReadBody, verifies the sender signature before it parses anything, then
// opens a transaction, calls Record with the delivery ID and, only when
// Record reports a new delivery, stores the effects of the event in the same
// transaction. It answers 2xx after the commit. A repeated delivery ID
// changes nothing and also gets 2xx.
package webhook

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/delinoio/mono/servers/sandrun-api/internal/db"
	"github.com/delinoio/mono/servers/sandrun-api/internal/jobs"
)

// MaxBodyBytes bounds a webhook body (1 MiB).
const MaxBodyBytes = 1 << 20

// Retention is how long delivery IDs are kept.
const Retention = 7 * 24 * time.Hour

// Sender is a webhook sender, as stored in webhook_deliveries.sender.
type Sender int16

const (
	SenderUnspecified Sender = iota
	SenderGitHub
	SenderOpenAI
)

// Label returns the `sender` label of sandrun_webhook_deliveries_total.
func (s Sender) Label() string {
	switch s {
	case SenderGitHub:
		return "github"
	case SenderOpenAI:
		return "openai"
	}
	return "unknown"
}

// Result is the `result` label of sandrun_webhook_deliveries_total.
type Result string

const (
	// ResultProcessed: the event changed SandRun's state.
	ResultProcessed Result = "processed"
	// ResultIgnored: a valid delivery that SandRun does not act on.
	ResultIgnored Result = "ignored"
	// ResultDuplicate: a repeated delivery ID.
	ResultDuplicate Result = "duplicate"
	// ResultInvalidSignature: a missing or invalid signature (401).
	ResultInvalidSignature Result = "invalid_signature"
	// ResultInvalidPayload: a signed delivery without a delivery ID or with
	// a body that does not parse (400).
	ResultInvalidPayload Result = "invalid_payload"
	// ResultTooLarge: a body over MaxBodyBytes (413).
	ResultTooLarge Result = "too_large"
	// ResultNotConfigured: the endpoint's sender is not configured (503).
	ResultNotConfigured Result = "not_configured"
	// ResultFailed: an internal error (500); the sender may redeliver.
	ResultFailed Result = "failed"
)

// ErrTooLarge reports a body over MaxBodyBytes.
var ErrTooLarge = errors.New("webhook body too large")

// ReadBody reads the raw body, at most MaxBodyBytes.
func ReadBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return nil, ErrTooLarge
	}
	return body, err
}

// Record stores a delivery ID of a sender in the caller's transaction. It
// reports false when the ID was recorded before: the caller then changes
// nothing and answers 2xx.
func Record(ctx context.Context, q db.Querier, sender Sender, deliveryID string, now time.Time) (bool, error) {
	tag, err := q.Exec(ctx, "INSERT INTO webhook_deliveries (sender, delivery_id, receive_time) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING",
		int16(sender), deliveryID, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Seen reports whether a delivery ID of a sender is recorded.
func Seen(ctx context.Context, q db.Querier, sender Sender, deliveryID string) (bool, error) {
	var n int
	err := q.QueryRow(ctx, "SELECT count(*) FROM webhook_deliveries WHERE sender = $1 AND delivery_id = $2", int16(sender), deliveryID).Scan(&n)
	return n > 0, err
}

// RetentionJob is the daily job that deletes delivery IDs older than
// Retention.
func RetentionJob(d *db.DB) jobs.Job {
	return jobs.SQLJob(d, "webhook_deliveries", "DELETE FROM webhook_deliveries WHERE receive_time < $1::timestamptz - interval '7 days'")
}
