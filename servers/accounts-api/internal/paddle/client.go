// SPDX-License-Identifier: FSL-1.1-ALv2

// Package paddle is the minimal hand-written client of the Paddle Billing API
// that accounts-api uses, and the verification and parsing of Paddle webhook
// deliveries (docs/servers-accounts-api-foundation.md, Payments and credits
// and Paddle webhook). It uses only the Go standard library.
//
// API reference: https://developer.paddle.com/api-reference/overview. Every
// request sends `Authorization: Bearer <API key>` and `Paddle-Version: 1`
// (https://developer.paddle.com/api-reference/about/versioning).
package paddle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Environment is a Paddle environment.
type Environment string

const (
	// EnvironmentSandbox is the Paddle sandbox, for development and tests.
	EnvironmentSandbox Environment = "sandbox"
	// EnvironmentProduction is the live Paddle environment.
	EnvironmentProduction Environment = "production"
)

// ParseEnvironment returns the environment of a configuration value.
func ParseEnvironment(value string) (Environment, bool) {
	switch Environment(value) {
	case EnvironmentSandbox, EnvironmentProduction:
		return Environment(value), true
	default:
		return "", false
	}
}

// BaseURL returns the API base URL of the environment.
func (e Environment) BaseURL() string {
	if e == EnvironmentProduction {
		return "https://api.paddle.com"
	}
	return "https://sandbox-api.paddle.com"
}

// apiVersion is the Paddle API version that the client speaks.
const apiVersion = "1"

// maxResponseBytes bounds Paddle responses.
const maxResponseBytes = 1 << 20

// Config configures the client.
type Config struct {
	// BaseURL is the API base URL, normally Environment.BaseURL(); tests use
	// a fake.
	BaseURL string
	// APIKey is the Paddle API key. Secret: never log it.
	APIKey string
	Client *http.Client
}

// Client calls the Paddle Billing API.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// New returns a client.
func New(cfg Config) *Client {
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{baseURL: strings.TrimRight(cfg.BaseURL, "/"), apiKey: cfg.APIKey, http: cfg.Client}
}

// APIError is an error response of the Paddle API
// (https://developer.paddle.com/api-reference/about/errors). It keeps only the
// status, the error type and code and the request ID; the detail text is
// dropped so that no payment detail can reach a log or a client.
type APIError struct {
	Status    int
	Type      string
	Code      string
	RequestID string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("paddle api status %d code %q", e.Status, e.Code)
}

// Error codes that the server distinguishes
// (https://developer.paddle.com/errors/overview).
const (
	// CodeNotFound means the requested entity does not exist.
	CodeNotFound = "not_found"
	// CodePaymentDeclined: a one-time charge with `on_payment_failure:
	// prevent_change` whose immediate payment failed
	// (https://developer.paddle.com/errors/subscriptions/subscription_payment_declined).
	CodePaymentDeclined = "subscription_payment_declined"
	// CodeUpdateWhenPastDue: changes and one-time charges are not allowed
	// while the subscription is past_due.
	CodeUpdateWhenPastDue = "subscription_update_when_past_due"
	// CodeNextBilledAtTooSoon: changes are not allowed when the next billing
	// period starts within 30 minutes.
	CodeNextBilledAtTooSoon = "subscription_next_billed_at_too_soon"
)

// Outcome classifies the result of a request that may change state at Paddle.
type Outcome int

const (
	// OutcomeOK: Paddle accepted the request.
	OutcomeOK Outcome = iota
	// OutcomeRejected: Paddle rejected the request (a 4xx answer other than
	// 401, 403, 404 and 429). Nothing changed at Paddle.
	OutcomeRejected
	// OutcomeNotApplied: Paddle did not process the request (401, 403, 429,
	// or a request that could not be built or sent). Nothing changed at
	// Paddle.
	OutcomeNotApplied
	// OutcomeUnknown: the result is unknown (a 5xx answer, a timeout or a
	// broken connection after the request was sent). Paddle may or may not
	// have applied it.
	OutcomeUnknown
	// OutcomeNotFound: the entity does not exist (404).
	OutcomeNotFound
)

// Classify returns the outcome of err from a client call.
func Classify(err error) Outcome {
	if err == nil {
		return OutcomeOK
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.Status == http.StatusNotFound:
			return OutcomeNotFound
		case apiErr.Status == http.StatusUnauthorized, apiErr.Status == http.StatusForbidden,
			apiErr.Status == http.StatusTooManyRequests:
			return OutcomeNotApplied
		case apiErr.Status >= 400 && apiErr.Status < 500:
			return OutcomeRejected
		default:
			return OutcomeUnknown
		}
	}
	var notSent *notSentError
	if errors.As(err, &notSent) {
		return OutcomeNotApplied
	}
	return OutcomeUnknown
}

// ErrorCode returns the Paddle error code of err, or "".
func ErrorCode(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return ""
}

// notSentError marks an error before the request reached the network.
type notSentError struct{ err error }

func (e *notSentError) Error() string { return e.err.Error() }
func (e *notSentError) Unwrap() error { return e.err }

type errorEnvelope struct {
	Error struct {
		Type string `json:"type"`
		Code string `json:"code"`
	} `json:"error"`
	Meta struct {
		RequestID string `json:"request_id"`
	} `json:"meta"`
}

// do sends one request and decodes the `data` member of a 2xx answer into
// out.
func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return &notSentError{err: fmt.Errorf("encode paddle request: %w", err)}
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return &notSentError{err: fmt.Errorf("build paddle request: %w", err)}
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Paddle-Version", apiVersion)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// The URL error can name the base URL but never the API key, which
		// travels only in a header.
		return fmt.Errorf("paddle request %s %s: %w", method, redactPath(path), err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			// The status line already decides the outcome (for example a 429
			// is never applied and a 5xx stays unknown), so keep it and
			// discard the partial body: it never reaches the error, the
			// code, the type or the request ID.
			return &APIError{Status: resp.StatusCode}
		}
		return fmt.Errorf("read paddle response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var env errorEnvelope
		_ = json.Unmarshal(raw, &env)
		return &APIError{Status: resp.StatusCode, Type: env.Error.Type, Code: env.Error.Code, RequestID: env.Meta.RequestID}
	}
	if out == nil {
		return nil
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	// A JSON null entity is as absent as a missing data member: decoding it
	// into out is a no-op that would return a zero entity as a success.
	if err := json.Unmarshal(raw, &env); err != nil || len(env.Data) == 0 || bytes.Equal(bytes.TrimSpace(env.Data), []byte("null")) {
		return errors.New("paddle response is not valid")
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return errors.New("paddle response data is not valid")
	}
	return nil
}

// redactPath keeps only the first path segment for error texts.
func redactPath(path string) string {
	segment, _, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	return "/" + segment
}

// CustomData is the Paddle `custom_data` object. accounts-api writes only
// string values.
type CustomData map[string]string

// Custom data keys that accounts-api writes.
const (
	// CustomDataBillingAccountID holds the billing account ID on checkout
	// transactions (and on the subscriptions that Paddle creates from them).
	CustomDataBillingAccountID = "billing_account_id"
	// CustomDataTopUpID holds the top-up ID on the non-catalog price of a
	// one-time charge, so the resulting transaction can be matched to the
	// top-up.
	CustomDataTopUpID = "top_up_id"
)

// Transaction is the part of a Paddle transaction entity that accounts-api
// reads (https://developer.paddle.com/api-reference/transactions/overview).
type Transaction struct {
	ID             string            `json:"id"`
	Status         string            `json:"status"`
	CollectionMode string            `json:"collection_mode"`
	CustomerID     string            `json:"customer_id"`
	SubscriptionID string            `json:"subscription_id"`
	Origin         string            `json:"origin"`
	CustomData     map[string]any    `json:"custom_data"`
	CurrencyCode   string            `json:"currency_code"`
	Items          []TransactionItem `json:"items"`
	Details        struct {
		Totals Totals `json:"totals"`
	} `json:"details"`
	BillingPeriod *Period    `json:"billing_period"`
	BilledAt      *time.Time `json:"billed_at"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// TransactionItem is one item of a transaction.
type TransactionItem struct {
	Price    Price `json:"price"`
	Quantity int64 `json:"quantity"`
}

// Price is the part of a price entity that accounts-api reads.
type Price struct {
	ID         string         `json:"id"`
	ProductID  string         `json:"product_id"`
	CustomData map[string]any `json:"custom_data"`
}

// Totals are the transaction totals. Paddle sends amounts as strings in the
// lowest denomination of the currency (cents for USD).
type Totals struct {
	Subtotal     string `json:"subtotal"`
	Discount     string `json:"discount"`
	Tax          string `json:"tax"`
	Total        string `json:"total"`
	CurrencyCode string `json:"currency_code"`
}

// Period is a billing period.
type Period struct {
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
}

// ParseCents parses a Paddle amount string in cents. Empty is 0.
func ParseCents(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	cents, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("paddle amount %q is not an integer", value)
	}
	return cents, nil
}

// CustomString returns the string value of key in custom data, or "".
func CustomString(data map[string]any, key string) string {
	value, _ := data[key].(string)
	return value
}

// Subscription is the part of a Paddle subscription entity that accounts-api
// reads (https://developer.paddle.com/api-reference/subscriptions/overview).
type Subscription struct {
	ID                   string           `json:"id"`
	Status               string           `json:"status"`
	CustomerID           string           `json:"customer_id"`
	CustomData           map[string]any   `json:"custom_data"`
	CurrentBillingPeriod *Period          `json:"current_billing_period"`
	NextBilledAt         *time.Time       `json:"next_billed_at"`
	ScheduledChange      *ScheduledChange `json:"scheduled_change"`
	CanceledAt           *time.Time       `json:"canceled_at"`
	UpdatedAt            time.Time        `json:"updated_at"`
	// TransactionID is present only in the `subscription.created` webhook:
	// the transaction that created the subscription.
	TransactionID string `json:"transaction_id"`
}

// ScheduledChange is a change that Paddle applies later, such as a
// cancellation at the end of the billing period.
type ScheduledChange struct {
	Action      string    `json:"action"`
	EffectiveAt time.Time `json:"effective_at"`
}

// Subscription status values of Paddle.
const (
	SubscriptionStatusActive   = "active"
	SubscriptionStatusTrialing = "trialing"
	SubscriptionStatusPastDue  = "past_due"
	SubscriptionStatusPaused   = "paused"
	SubscriptionStatusCanceled = "canceled"
)

// CancelsAtPeriodEnd reports whether a cancellation is scheduled.
func (s Subscription) CancelsAtPeriodEnd() bool {
	return s.ScheduledChange != nil && s.ScheduledChange.Action == "cancel"
}

// CreateCheckoutTransaction creates an automatically collected transaction
// for one unit of priceID with custom data, to open with Paddle.js checkout
// (`POST /transactions`,
// https://developer.paddle.com/api-reference/transactions/create-transaction
// and https://developer.paddle.com/build/transactions/pass-transaction-checkout).
// Without a customer and an address the transaction is a draft; the checkout
// collects them.
func (c *Client) CreateCheckoutTransaction(ctx context.Context, priceID string, custom CustomData) (Transaction, error) {
	body := map[string]any{
		"items":           []map[string]any{{"price_id": priceID, "quantity": 1}},
		"collection_mode": CollectionModeAutomatic,
		"custom_data":     custom,
	}
	var txn Transaction
	if err := c.do(ctx, http.MethodPost, "/transactions", body, &txn); err != nil {
		return txn, err
	}
	// A 2xx answer whose transaction has no ID cannot open a checkout and
	// must not be stored. The answer is as malformed as a body that is not
	// JSON, so Classify reports an unknown outcome, like the decoder errors.
	if txn.ID == "" {
		return Transaction{}, errors.New("paddle response is not valid")
	}
	return txn, nil
}

// CollectionModeAutomatic lets Paddle collect payment through checkout.
const CollectionModeAutomatic = "automatic"

// Transaction status values of Paddle
// (https://developer.paddle.com/api-reference/transactions/overview).
const (
	TransactionStatusDraft     = "draft"
	TransactionStatusReady     = "ready"
	TransactionStatusBilled    = "billed"
	TransactionStatusPaid      = "paid"
	TransactionStatusCompleted = "completed"
	TransactionStatusCanceled  = "canceled"
	TransactionStatusPastDue   = "past_due"
)

// GetTransaction reads a transaction (`GET /transactions/{id}`).
func (c *Client) GetTransaction(ctx context.Context, id string) (Transaction, error) {
	var txn Transaction
	err := c.do(ctx, http.MethodGet, "/transactions/"+url.PathEscape(id), nil, &txn)
	return txn, err
}

// CancelTransaction cancels a transaction that has not been paid (`PATCH
// /transactions/{id}` with `status: canceled`,
// https://developer.paddle.com/api-reference/transactions/update-transaction).
// Paddle allows the change for the statuses `draft`, `ready`, `billed` and
// `past_due`; it rejects a completed transaction. The caller must check that
// the answer names the transaction and has the status `canceled`: a 2xx answer
// is not a confirmation by itself.
func (c *Client) CancelTransaction(ctx context.Context, id string) (Transaction, error) {
	var txn Transaction
	err := c.do(ctx, http.MethodPatch, "/transactions/"+url.PathEscape(id),
		map[string]any{"status": TransactionStatusCanceled}, &txn)
	return txn, err
}

// GetSubscription reads a subscription (`GET /subscriptions/{id}`).
func (c *Client) GetSubscription(ctx context.Context, id string) (Subscription, error) {
	var sub Subscription
	err := c.do(ctx, http.MethodGet, "/subscriptions/"+url.PathEscape(id), nil, &sub)
	return sub, err
}

// EffectiveFrom is when a cancellation takes effect.
type EffectiveFrom string

const (
	// EffectiveNextBillingPeriod cancels at the end of the paid period.
	EffectiveNextBillingPeriod EffectiveFrom = "next_billing_period"
	// EffectiveImmediately cancels now.
	EffectiveImmediately EffectiveFrom = "immediately"
)

// CancelSubscription cancels a subscription (`POST
// /subscriptions/{id}/cancel`,
// https://developer.paddle.com/build/subscriptions/cancel-subscriptions). With
// EffectiveNextBillingPeriod Paddle schedules a change with the action
// `cancel` and the subscription stays active until the period ends.
func (c *Client) CancelSubscription(ctx context.Context, id string, when EffectiveFrom) (Subscription, error) {
	var sub Subscription
	err := c.do(ctx, http.MethodPost, "/subscriptions/"+url.PathEscape(id)+"/cancel",
		map[string]any{"effective_from": string(when)}, &sub)
	return sub, err
}

// RemoveScheduledChange removes a scheduled cancellation (`PATCH
// /subscriptions/{id}` with `scheduled_change: null`,
// https://developer.paddle.com/build/subscriptions/cancel-subscriptions).
func (c *Client) RemoveScheduledChange(ctx context.Context, id string) (Subscription, error) {
	var sub Subscription
	err := c.do(ctx, http.MethodPatch, "/subscriptions/"+url.PathEscape(id),
		map[string]any{"scheduled_change": nil}, &sub)
	return sub, err
}

// Charge is a one-time charge of a non-catalog price on an existing
// product.
type Charge struct {
	ProductID string
	// AmountCents is the tax-exclusive price in USD cents.
	AmountCents int64
	// Name and Description label the line item; Description has 2 to 200
	// characters.
	Name        string
	Description string
	// CustomData is stored on the non-catalog price and comes back in
	// `items[].price.custom_data` of the transaction that the charge creates.
	CustomData CustomData
}

// CreateOneTimeCharge bills a one-time charge on a subscription now (`POST
// /subscriptions/{id}/charge`,
// https://developer.paddle.com/api-reference/subscriptions/create-one-time-charge
// and https://developer.paddle.com/build/subscriptions/bill-add-one-time-charge).
//
//   - `effective_from: immediately` creates a transaction with the origin
//     `subscription_charge` at once, and Paddle collects it at once with the
//     payment method on file for the subscription.
//   - `on_payment_failure: prevent_change` (the Paddle default, sent
//     explicitly) makes a failed payment fail the request with
//     `subscription_payment_declined`, so the failure is known in the
//     response.
//   - Paddle rejects the charge while the subscription is `past_due` and
//     when the next billing period starts within 30 minutes.
//   - The response is the subscription entity; the charge itself is not in
//     it. accounts-api matches the transaction of the charge through the
//     `top_up_id` custom data of the non-catalog price.
//   - `tax_mode: external` makes the price tax exclusive: Paddle adds the tax
//     on top.
func (c *Client) CreateOneTimeCharge(ctx context.Context, subscriptionID string, charge Charge) (Subscription, error) {
	body := map[string]any{
		"effective_from":     "immediately",
		"on_payment_failure": "prevent_change",
		"items": []map[string]any{{
			"quantity": 1,
			"price": map[string]any{
				"product_id":  charge.ProductID,
				"name":        charge.Name,
				"description": charge.Description,
				"tax_mode":    "external",
				"unit_price": map[string]any{
					"amount":        strconv.FormatInt(charge.AmountCents, 10),
					"currency_code": "USD",
				},
				"custom_data": charge.CustomData,
			},
		}},
	}
	var sub Subscription
	err := c.do(ctx, http.MethodPost, "/subscriptions/"+url.PathEscape(subscriptionID)+"/charge", body, &sub)
	return sub, err
}

// InvoiceURL returns the temporary link to the invoice PDF of a transaction
// (`GET /transactions/{id}/invoice`,
// https://developer.paddle.com/api-reference/transactions/get-invoice-pdf). The
// link expires after an hour. Never log it.
func (c *Client) InvoiceURL(ctx context.Context, transactionID string) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	if err := c.do(ctx, http.MethodGet, "/transactions/"+url.PathEscape(transactionID)+"/invoice", nil, &out); err != nil {
		return "", err
	}
	if out.URL == "" {
		return "", errors.New("paddle invoice response has no url")
	}
	return out.URL, nil
}
