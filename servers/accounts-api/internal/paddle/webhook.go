// SPDX-License-Identifier: FSL-1.1-ALv2

package paddle

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// SignatureHeader is the header of a signed webhook delivery.
const SignatureHeader = "Paddle-Signature"

// SignatureTolerance is the largest accepted difference between the
// signature timestamp and the server time
// (docs/servers-accounts-api-foundation.md, Paddle webhook).
const SignatureTolerance = 5 * time.Minute

// Signature verification errors. They name no secret and no signature.
var (
	ErrSignatureMissing   = errors.New("paddle signature missing")
	ErrSignatureMalformed = errors.New("paddle signature malformed")
	ErrSignatureStale     = errors.New("paddle signature timestamp outside the tolerance")
	ErrSignatureMismatch  = errors.New("paddle signature mismatch")
)

// VerifySignature checks the `Paddle-Signature` header of a delivery
// (https://developer.paddle.com/webhooks/signature-verification): the header
// is `ts=<unix time>;h1=<hex HMAC-SHA256>`, the signed payload is `<ts>:<raw
// body>` and the key is the secret of the notification destination. The
// header may carry several `h1` values while Paddle rotates secrets; one
// match is enough. The comparison runs in constant time, and a timestamp
// more than tolerance away from now is rejected.
func VerifySignature(header string, body []byte, secret string, now time.Time, tolerance time.Duration) error {
	if header == "" {
		return ErrSignatureMissing
	}
	var ts string
	var signatures [][]byte
	for _, part := range strings.Split(header, ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return ErrSignatureMalformed
		}
		switch key {
		case "ts":
			ts = value
		case "h1":
			sig, err := hex.DecodeString(value)
			if err != nil || len(sig) != sha256.Size {
				return ErrSignatureMalformed
			}
			signatures = append(signatures, sig)
		}
	}
	if ts == "" || len(signatures) == 0 {
		return ErrSignatureMalformed
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrSignatureMalformed
	}
	diff := now.Sub(time.Unix(unix, 0))
	if diff > tolerance || diff < -tolerance {
		return ErrSignatureStale
	}
	want := Sign(secret, ts, body)
	for _, sig := range signatures {
		if hmac.Equal(sig, want) {
			return nil
		}
	}
	return ErrSignatureMismatch
}

// Sign returns the HMAC-SHA256 of `<ts>:<body>` with secret.
func Sign(secret, ts string, body []byte) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte(":"))
	mac.Write(body)
	return mac.Sum(nil)
}

// SignatureHeaderValue returns a `Paddle-Signature` value for body at time
// ts (tests and the fake).
func SignatureHeaderValue(secret string, ts time.Time, body []byte) string {
	unix := strconv.FormatInt(ts.Unix(), 10)
	return "ts=" + unix + ";h1=" + hex.EncodeToString(Sign(secret, unix, body))
}

// Event is the envelope of a webhook notification
// (https://developer.paddle.com/webhooks/overview): `event_id` (`evt_...`),
// `event_type` (`entity.event`), `occurred_at`, `notification_id` and the
// entity in `data`.
type Event struct {
	EventID        string          `json:"event_id"`
	EventType      string          `json:"event_type"`
	OccurredAt     time.Time       `json:"occurred_at"`
	NotificationID string          `json:"notification_id"`
	Data           json.RawMessage `json:"data"`
}

// Event types that accounts-api applies.
const (
	// EventTransactionCompleted: a transaction is paid and Paddle finished
	// processing it
	// (https://developer.paddle.com/webhooks/transactions/transaction-completed).
	EventTransactionCompleted = "transaction.completed"
	// EventTransactionPaymentFailed: a payment attempt of a transaction
	// failed.
	EventTransactionPaymentFailed = "transaction.payment_failed"
	// EventAdjustmentCreated and EventAdjustmentUpdated: refunds and
	// chargebacks. A refund that needs approval is created as
	// `pending_approval` and updated to `approved` later
	// (https://developer.paddle.com/webhooks/adjustments/adjustment-updated).
	EventAdjustmentCreated = "adjustment.created"
	EventAdjustmentUpdated = "adjustment.updated"
)

// IsSubscriptionEvent reports whether eventType carries a subscription entity
// (`subscription.created`, `.activated`, `.updated`, `.past_due`, `.paused`,
// `.resumed`, `.canceled`, `.trialing`, `.imported`).
func IsSubscriptionEvent(eventType string) bool {
	return strings.HasPrefix(eventType, "subscription.")
}

// ParseEvent decodes the envelope of a verified delivery.
func ParseEvent(body []byte) (Event, error) {
	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil {
		return Event{}, errors.New("paddle event is not valid JSON")
	}
	if ev.EventID == "" || ev.EventType == "" || len(ev.Data) == 0 {
		return Event{}, errors.New("paddle event lacks event_id, event_type or data")
	}
	return ev, nil
}

// Adjustment is the part of a Paddle adjustment entity that accounts-api
// reads (https://developer.paddle.com/api-reference/adjustments/overview).
type Adjustment struct {
	ID             string `json:"id"`
	Action         string `json:"action"`
	TransactionID  string `json:"transaction_id"`
	SubscriptionID string `json:"subscription_id"`
	Status         string `json:"status"`
	CurrencyCode   string `json:"currency_code"`
	Totals         struct {
		Subtotal     string `json:"subtotal"`
		Tax          string `json:"tax"`
		Total        string `json:"total"`
		CurrencyCode string `json:"currency_code"`
	} `json:"totals"`
}

// Adjustment actions and statuses.
const (
	AdjustmentActionRefund     = "refund"
	AdjustmentActionChargeback = "chargeback"
	AdjustmentStatusApproved   = "approved"
)

// Transaction origins
// (https://developer.paddle.com/webhooks/transactions/transaction-completed).
const (
	OriginWeb                   = "web"
	OriginAPI                   = "api"
	OriginSubscriptionCharge    = "subscription_charge"
	OriginSubscriptionRecurring = "subscription_recurring"
)
