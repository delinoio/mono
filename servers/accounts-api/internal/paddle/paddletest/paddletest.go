// SPDX-License-Identifier: FSL-1.1-ALv2

// Package paddletest is a fake Paddle Billing API for tests, and builders of
// signed webhook deliveries. The fake keeps subscriptions in memory, records
// every request and can be told to fail one-time charges and other calls.
package paddletest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
)

// APIKey is the API key that the fake accepts.
const APIKey = "pdl_test_api_key"

// Request is one recorded request.
type Request struct {
	Method string
	Path   string
	Body   map[string]any
}

// Failure makes a call fail with a Paddle error answer.
type Failure struct {
	Status int
	Code   string
	// Hang makes the fake wait for the client to give up instead of
	// answering, which a client sees as an unknown outcome.
	Hang bool
	// Truncate answers with the status and the first bytes of a body, then
	// breaks the connection, so a client fails while it reads the body.
	Truncate bool
}

// rawResponse is a verbatim answer that replaces the normal handling of a
// request.
type rawResponse struct {
	status int
	body   string
}

// Server is the fake Paddle API.
type Server struct {
	URL string

	mu            sync.Mutex
	requests      []Request
	subscriptions map[string]*paddle.Subscription
	chargeFailure *Failure
	failures      map[string]*Failure
	raw           map[string]rawResponse
	seq           atomic.Int64
}

// New starts a fake Paddle API that stops when the test ends.
func New(t testing.TB) *Server {
	s := &Server{subscriptions: map[string]*paddle.Subscription{}, failures: map[string]*Failure{}, raw: map[string]rawResponse{}}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

// NewID returns a Paddle-style ID with prefix, such as `txn_...`.
func (s *Server) NewID(prefix string) string {
	return fmt.Sprintf("%s_%026d", prefix, s.seq.Add(1))
}

// SetSubscription stores a subscription as Paddle would return it.
func (s *Server) SetSubscription(sub paddle.Subscription) {
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := sub
	s.subscriptions[sub.ID] = &copied
}

// Subscription returns the stored subscription.
func (s *Server) Subscription(id string) (paddle.Subscription, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.subscriptions[id]
	if !ok {
		return paddle.Subscription{}, false
	}
	return *sub, true
}

// FailCharges makes every following one-time charge fail with f; nil
// restores success.
func (s *Server) FailCharges(f *Failure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chargeFailure = f
}

// Fail makes every following request whose `<method> <path prefix>` matches
// key fail with f, for example `POST /transactions`; nil removes it.
func (s *Server) Fail(key string, f *Failure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f == nil {
		delete(s.failures, key)
		return
	}
	s.failures[key] = f
}

// Respond makes every following request whose `<method> <path prefix>`
// matches key answer with the status and the verbatim body, for example
// `POST /transactions` with `{"data":null}`, to exercise malformed success
// answers. A failure set with Fail or FailCharges takes precedence. An empty
// body removes the override.
func (s *Server) Respond(key string, status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if body == "" {
		delete(s.raw, key)
		return
	}
	s.raw[key] = rawResponse{status: status, body: body}
}

// Requests returns the recorded requests in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Charges returns the recorded one-time charge requests.
func (s *Server) Charges() []Request {
	var out []Request
	for _, r := range s.Requests() {
		if r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/charge") {
			out = append(out, r)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, r *http.Request, f *Failure) {
	if f.Hang {
		<-r.Context().Done()
		return
	}
	if f.Truncate {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(f.Status)
		_, _ = w.Write([]byte("{"))
		w.(http.Flusher).Flush()
		// Aborting the handler closes the connection before the declared
		// length is written; net/http recovers this panic without a log.
		panic(http.ErrAbortHandler)
	}
	writeJSON(w, f.Status, map[string]any{
		"error": map[string]any{"type": "request_error", "code": f.Code, "detail": "fake failure"},
		"meta":  map[string]any{"request_id": "req_fake"},
	})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+APIKey || r.Header.Get("Paddle-Version") != "1" {
		writeError(w, r, &Failure{Status: http.StatusForbidden, Code: "forbidden"})
		return
	}
	var body map[string]any
	if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	s.mu.Lock()
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, Body: body})
	var failure *Failure
	for key, f := range s.failures {
		method, prefix, _ := strings.Cut(key, " ")
		if method == r.Method && strings.HasPrefix(r.URL.Path, prefix) {
			failure = f
		}
	}
	if failure == nil && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/charge") {
		failure = s.chargeFailure
	}
	var override *rawResponse
	if failure == nil {
		for key, o := range s.raw {
			method, prefix, _ := strings.Cut(key, " ")
			if method == r.Method && strings.HasPrefix(r.URL.Path, prefix) {
				o := o
				override = &o
			}
		}
	}
	s.mu.Unlock()
	if failure != nil {
		writeError(w, r, failure)
		return
	}
	if override != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(override.status)
		_, _ = io.WriteString(w, override.body)
		return
	}

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/transactions":
		writeJSON(w, http.StatusCreated, map[string]any{"data": map[string]any{
			"id": s.NewID("txn"), "status": "draft", "custom_data": body["custom_data"],
			"created_at": time.Now().UTC(), "updated_at": time.Now().UTC(),
		}})
	case len(parts) == 3 && parts[0] == "transactions" && parts[2] == "invoice" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
			"url": "https://paddle.example.test/invoices/" + parts[1] + ".pdf?token=secret",
		}})
	case len(parts) >= 2 && parts[0] == "subscriptions":
		s.serveSubscription(w, r, parts, body)
	default:
		writeError(w, r, &Failure{Status: http.StatusNotFound, Code: "not_found"})
	}
}

func (s *Server) serveSubscription(w http.ResponseWriter, r *http.Request, parts []string, body map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.subscriptions[parts[1]]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"type": "request_error", "code": "not_found"}})
		return
	}
	now := time.Now().UTC()
	switch {
	case len(parts) == 2 && r.Method == http.MethodGet:
	case len(parts) == 2 && r.Method == http.MethodPatch:
		if v, present := body["scheduled_change"]; present && v == nil {
			sub.ScheduledChange = nil
			sub.UpdatedAt = now
		}
	case len(parts) == 3 && parts[2] == "cancel" && r.Method == http.MethodPost:
		if body["effective_from"] == string(paddle.EffectiveImmediately) {
			sub.Status = paddle.SubscriptionStatusCanceled
			sub.CanceledAt = &now
			sub.CurrentBillingPeriod = nil
			sub.ScheduledChange = nil
		} else {
			at := now.Add(30 * 24 * time.Hour)
			if sub.CurrentBillingPeriod != nil {
				at = sub.CurrentBillingPeriod.EndsAt
			}
			sub.ScheduledChange = &paddle.ScheduledChange{Action: "cancel", EffectiveAt: at}
		}
		sub.UpdatedAt = now
	case len(parts) == 3 && parts[2] == "charge" && r.Method == http.MethodPost:
		if sub.Status == paddle.SubscriptionStatusPastDue {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"type": "request_error", "code": paddle.CodeUpdateWhenPastDue}})
			return
		}
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"type": "request_error", "code": "not_found"}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": sub})
}

// ChargeTopUpID returns the `top_up_id` custom data of a recorded charge.
func ChargeTopUpID(r Request) string {
	items, _ := r.Body["items"].([]any)
	if len(items) == 0 {
		return ""
	}
	item, _ := items[0].(map[string]any)
	price, _ := item["price"].(map[string]any)
	custom, _ := price["custom_data"].(map[string]any)
	id, _ := custom[paddle.CustomDataTopUpID].(string)
	return id
}

// ChargeAmountCents returns the unit price of a recorded charge.
func ChargeAmountCents(r Request) string {
	items, _ := r.Body["items"].([]any)
	if len(items) == 0 {
		return ""
	}
	item, _ := items[0].(map[string]any)
	price, _ := item["price"].(map[string]any)
	unit, _ := price["unit_price"].(map[string]any)
	amount, _ := unit["amount"].(string)
	return amount
}

// Event builds the JSON body of a webhook notification.
func Event(eventID, eventType string, occurredAt time.Time, data any) []byte {
	body, err := json.Marshal(map[string]any{
		"event_id":        eventID,
		"event_type":      eventType,
		"occurred_at":     occurredAt.UTC().Format(time.RFC3339Nano),
		"notification_id": "ntf_" + strings.TrimPrefix(eventID, "evt_"),
		"data":            data,
	})
	if err != nil {
		panic(err)
	}
	return body
}

// TransactionData is the data of a transaction event.
type TransactionData struct {
	ID             string
	SubscriptionID string
	Origin         string
	// BillingAccountID goes into the transaction custom data when set.
	BillingAccountID string
	// PriceID is the catalog price of the item (subscription payments).
	PriceID string
	// TopUpID goes into the custom data of the item price (one-time
	// charges).
	TopUpID       string
	SubtotalCents int64
	TaxCents      int64
	Currency      string
	PeriodEnd     time.Time
	Status        string
}

// Transaction returns the data object of a transaction event.
func Transaction(d TransactionData) map[string]any {
	if d.Currency == "" {
		d.Currency = "USD"
	}
	if d.Status == "" {
		d.Status = "completed"
	}
	priceID := d.PriceID
	if priceID == "" {
		priceID = "pri_nc_" + d.ID
	}
	price := map[string]any{"id": priceID, "product_id": "pro_fake"}
	if d.TopUpID != "" {
		price["custom_data"] = map[string]any{paddle.CustomDataTopUpID: d.TopUpID}
	}
	data := map[string]any{
		"id":            d.ID,
		"status":        d.Status,
		"origin":        d.Origin,
		"currency_code": d.Currency,
		"items":         []any{map[string]any{"price": price, "quantity": 1}},
		"details": map[string]any{"totals": map[string]any{
			"subtotal":      fmt.Sprint(d.SubtotalCents),
			"discount":      "0",
			"tax":           fmt.Sprint(d.TaxCents),
			"total":         fmt.Sprint(d.SubtotalCents + d.TaxCents),
			"currency_code": d.Currency,
		}},
		"created_at": time.Now().UTC(),
		"updated_at": time.Now().UTC(),
	}
	if d.SubscriptionID != "" {
		data["subscription_id"] = d.SubscriptionID
	}
	if d.BillingAccountID != "" {
		data["custom_data"] = map[string]any{paddle.CustomDataBillingAccountID: d.BillingAccountID}
	}
	if !d.PeriodEnd.IsZero() {
		data["billing_period"] = map[string]any{"starts_at": d.PeriodEnd.AddDate(0, -1, 0), "ends_at": d.PeriodEnd}
	}
	return data
}

// Adjustment returns the data object of an adjustment event.
func Adjustment(id, action, status, transactionID string, subtotalCents int64) map[string]any {
	return map[string]any{
		"id":             id,
		"action":         action,
		"status":         status,
		"transaction_id": transactionID,
		"currency_code":  "USD",
		"totals": map[string]any{
			"subtotal": fmt.Sprint(subtotalCents), "tax": "0", "total": fmt.Sprint(subtotalCents), "currency_code": "USD",
		},
	}
}
