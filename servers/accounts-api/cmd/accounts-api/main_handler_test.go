// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/protos/delino/accounts/v1/accountsv1connect"
	"github.com/delinoio/mono/servers/accounts-api/internal/config"
	"github.com/delinoio/mono/servers/accounts-api/internal/db"
)

// TestNewHandlerServesTheBillingServices checks the production wiring
// without a database: the services are mounted behind authentication and
// CORS, the Paddle webhook checks signatures, /metrics is not public and
// /healthz still answers.
func TestNewHandlerServesTheBillingServices(t *testing.T) {
	cfg, err := config.Load(func(name string) (string, bool) {
		value, ok := map[string]string{
			config.EnvDatabaseURL:             "postgres://user@127.0.0.1:1/none",
			config.EnvLogtoIssuer:             "http://127.0.0.1:1/oidc",
			config.EnvLogtoManagementEndpoint: "http://127.0.0.1:1",
			config.EnvLogtoM2MClientID:        "m2m",
			config.EnvLogtoM2MClientSecret:    "secret",
			config.EnvResendAPIKey:            "re_test",
			config.EnvEmailFrom:               "billing@delino.io",
			config.EnvPaddleEnvironment:       "sandbox",
			config.EnvPaddleAPIKey:            "pdl_test",
			config.EnvPaddleWebhookSecret:     "pdl_ntfset_test",
			config.EnvPaddlePriceID:           "pri_01aaaaaaaaaaaaaaaaaaaaaaaa",
			config.EnvPaddleTopUpProductID:    "pro_01bbbbbbbbbbbbbbbbbbbbbbbb",
		}[name]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(context.Background(), cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	handler, _, err := newHandler(cfg, database, slog.New(slog.NewJSONHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := accountsv1connect.NewBillingAccountServiceClient(http.DefaultClient, srv.URL)
	_, err = client.ListBillingAccounts(context.Background(), connect.NewRequest(&accountsv1.ListBillingAccountsRequest{}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("ListBillingAccounts without a token: %v, want unauthenticated", err)
	}
	credit := accountsv1connect.NewCreditServiceClient(http.DefaultClient, srv.URL)
	_, err = credit.GetBalance(context.Background(), connect.NewRequest(&accountsv1.GetBalanceRequest{}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("GetBalance without a token: %v, want unauthenticated", err)
	}

	// The Paddle webhook rejects an unsigned delivery before it reads the
	// database, and the public handler never serves the metrics.
	resp, err := http.Post(srv.URL+"/webhooks/paddle", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned webhook status %d, want 401", resp.StatusCode)
	}
	resp, err = http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("public /metrics status %d, want 404", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodOptions, srv.URL+accountsv1connect.BillingAccountServiceGetBillingAccountProcedure, nil)
	req.Header.Set("Origin", config.DefaultWebOrigin)
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != config.DefaultWebOrigin {
		t.Fatalf("preflight from the web origin: %d %v", resp.StatusCode, resp.Header)
	}

	resp, err = http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status %d", resp.StatusCode)
	}
}
