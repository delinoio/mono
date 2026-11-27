// SPDX-License-Identifier: FSL-1.1-ALv2

package paddletest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
)

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
