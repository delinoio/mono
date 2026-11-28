// SPDX-License-Identifier: FSL-1.1-ALv2

package paddletest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
)

func TestOverlappingRules(t *testing.T) {
	const broadKey = "GET /transactions/"
	const invoicePath = "/transactions/txn_demo/invoice"
	const narrowKey = "GET " + invoicePath
	const siblingPath = "/transactions/txn_other/invoice"

	for _, order := range []struct {
		name string
		keys [2]string
	}{
		{"broad first", [2]string{broadKey, narrowKey}},
		{"narrow first", [2]string{narrowKey, broadKey}},
	} {
		t.Run(order.name, func(t *testing.T) {
			t.Run("Fail", func(t *testing.T) {
				fake := New(t)
				failures := map[string]*Failure{
					broadKey:  {Status: http.StatusServiceUnavailable, Code: "fixture_broad"},
					narrowKey: {Status: http.StatusTooManyRequests, Code: "fixture_specific"},
				}
				for _, key := range order.keys {
					fake.Fail(key, failures[key])
				}
				for range 32 {
					assertRuleResponse(t, fake, http.MethodGet, invoicePath, "", http.StatusTooManyRequests, failureBody("fixture_specific"))
				}
				assertRuleResponse(t, fake, http.MethodGet, invoicePath+"-suffix", "", http.StatusTooManyRequests, failureBody("fixture_specific"))
				assertRuleResponse(t, fake, http.MethodGet, siblingPath, "", http.StatusServiceUnavailable, failureBody("fixture_broad"))

				fake.Fail(narrowKey, &Failure{Status: http.StatusBadGateway, Code: "replacement"})
				assertRuleResponse(t, fake, http.MethodGet, invoicePath, "", http.StatusBadGateway, failureBody("replacement"))
				fake.Fail(narrowKey, nil)
				assertRuleResponse(t, fake, http.MethodGet, invoicePath, "", http.StatusServiceUnavailable, failureBody("fixture_broad"))
			})

			t.Run("Respond", func(t *testing.T) {
				fake := New(t)
				responses := map[string]rawResponse{
					broadKey:  {status: http.StatusOK, body: `{"rule":"broad"}`},
					narrowKey: {status: http.StatusAccepted, body: `{"rule":"specific"}`},
				}
				for _, key := range order.keys {
					response := responses[key]
					fake.Respond(key, response.status, response.body)
				}
				for range 32 {
					assertRuleResponse(t, fake, http.MethodGet, invoicePath, "", http.StatusAccepted, `{"rule":"specific"}`)
				}
				assertRuleResponse(t, fake, http.MethodGet, invoicePath+"-suffix", "", http.StatusAccepted, `{"rule":"specific"}`)
				assertRuleResponse(t, fake, http.MethodGet, siblingPath, "", http.StatusOK, `{"rule":"broad"}`)

				fake.Respond(narrowKey, http.StatusCreated, `{"rule":"replacement"}`)
				assertRuleResponse(t, fake, http.MethodGet, invoicePath, "", http.StatusCreated, `{"rule":"replacement"}`)
				fake.Respond(narrowKey, 0, "")
				assertRuleResponse(t, fake, http.MethodGet, invoicePath, "", http.StatusOK, `{"rule":"broad"}`)
				fake.Respond(broadKey, 0, "")
				assertRuleResponse(t, fake, http.MethodGet, invoicePath, "", http.StatusOK,
					"{\"data\":{\"url\":\"https://paddle.example.test/invoices/txn_demo.pdf?token=secret\"}}\n")
			})
		})
	}
}

func TestRulePrecedenceAndRecording(t *testing.T) {
	const path = "/subscriptions/sub_demo/charge"
	const requestBody = `{"effective_from":"next_billing_period"}`
	fake := New(t)
	fake.SetSubscription(paddle.Subscription{ID: "sub_demo", Status: paddle.SubscriptionStatusActive})
	fake.Fail("POST /subscriptions/", &Failure{Status: http.StatusServiceUnavailable, Code: "explicit_failure"})
	fake.FailCharges(&Failure{Status: http.StatusTooManyRequests, Code: "charge_failure"})
	fake.Respond("POST "+path, http.StatusAccepted, `{"rule":"response"}`)

	assertRuleResponse(t, fake, http.MethodPost, path, requestBody, http.StatusServiceUnavailable, failureBody("explicit_failure"))
	assertRuleResponse(t, fake, http.MethodGet, path, "", http.StatusNotFound, "{\"error\":{\"code\":\"not_found\",\"type\":\"request_error\"}}\n")
	fake.Fail("POST /subscriptions/", nil)
	assertRuleResponse(t, fake, http.MethodPost, path, requestBody, http.StatusTooManyRequests, failureBody("charge_failure"))
	fake.FailCharges(nil)
	assertRuleResponse(t, fake, http.MethodPost, path, requestBody, http.StatusAccepted, `{"rule":"response"}`)

	post := Request{Method: http.MethodPost, Path: path, Body: map[string]any{"effective_from": "next_billing_period"}}
	want := []Request{post, {Method: http.MethodGet, Path: path}, post, post}
	if got := fake.Requests(); !reflect.DeepEqual(got, want) {
		t.Fatalf("recorded requests = %+v, want %+v", got, want)
	}
}

func assertRuleResponse(t *testing.T, fake *Server, method, path, requestBody string, wantStatus int, wantBody string) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, fake.URL+path, strings.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+APIKey)
	request.Header.Set("Paddle-Version", "1")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != wantStatus || string(body) != wantBody {
		t.Fatalf("%s %s returned status %d and body %q, want %d and %q",
			method, path, response.StatusCode, body, wantStatus, wantBody)
	}
}

func failureBody(code string) string {
	return `{"error":{"code":"` + code + `","detail":"fake failure","type":"request_error"},"meta":{"request_id":"req_fake"}}` + "\n"
}

func TestCreateTransactionKeepsDraftResponse(t *testing.T) {
	const id = "txn_00000000000000000000000001"
	for _, tc := range []struct {
		name   string
		status string
		update func(*testing.T, *Server)
	}{
		{
			name:   "setter",
			status: paddle.TransactionStatusCompleted,
			update: func(_ *testing.T, fake *Server) {
				fake.SetTransactionStatus(id, paddle.TransactionStatusCompleted)
			},
		},
		{
			name:   "cancel",
			status: paddle.TransactionStatusCanceled,
			update: func(t *testing.T, fake *Server) {
				response := httptest.NewRecorder()
				fake.serve(response, transactionRequest(http.MethodPatch, "/transactions/"+id, `{"status":"canceled"}`))
				if response.Code != http.StatusOK {
					t.Fatalf("PATCH status = %d, want %d", response.Code, http.StatusOK)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := New(t)
			request := transactionRequest(http.MethodPost, "/transactions",
				`{"collection_mode":"automatic","custom_data":{"billing_account_id":"acct_test"}}`)
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				fake.serve(response, request)
			}()
			defer func() { <-done }()

			// Coordinate the update with publication to the map. Waiting for
			// the POST handler here would serialize the accesses under test.
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, ok := fake.Transaction(id); ok {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("transaction was not published")
				}
				time.Sleep(time.Millisecond)
			}
			tc.update(t, fake)
			<-done

			if response.Code != http.StatusCreated {
				t.Fatalf("POST status = %d, want %d", response.Code, http.StatusCreated)
			}
			var body struct {
				Data paddle.Transaction `json:"data"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			created := body.Data
			if created.ID != id || created.Status != paddle.TransactionStatusDraft ||
				created.CollectionMode != "automatic" || len(created.CustomData) != 1 ||
				created.CustomData[paddle.CustomDataBillingAccountID] != "acct_test" {
				t.Fatalf("POST returned unexpected transaction: %+v", created)
			}
			if created.CreatedAt.IsZero() || !created.UpdatedAt.Equal(created.CreatedAt) {
				t.Fatalf("POST timestamps = %v and %v, want equal nonzero times", created.CreatedAt, created.UpdatedAt)
			}

			stored, ok := fake.Transaction(id)
			if !ok || stored.Status != tc.status || !stored.CreatedAt.Equal(created.CreatedAt) ||
				!stored.UpdatedAt.After(created.UpdatedAt) {
				t.Fatalf("stored transaction = %+v, found = %t, want %s with a newer update time", stored, ok, tc.status)
			}
		})
	}
}

func transactionRequest(method, path, body string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+APIKey)
	request.Header.Set("Paddle-Version", "1")
	return request
}
