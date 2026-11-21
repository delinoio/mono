// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/protos/delino/accounts/v1/accountsv1connect"
	"github.com/delinoio/mono/servers/accounts-api/internal/config"
	"github.com/delinoio/mono/servers/accounts-api/internal/db"
)

// TestNewHandlerServesBillingAccountService checks the production wiring
// without a database: the service is mounted behind authentication and CORS,
// and /healthz still answers.
func TestNewHandlerServesBillingAccountService(t *testing.T) {
	cfg, err := config.Load(func(name string) (string, bool) {
		value, ok := map[string]string{
			config.EnvDatabaseURL:             "postgres://user@127.0.0.1:1/none",
			config.EnvLogtoIssuer:             "http://127.0.0.1:1/oidc",
			config.EnvLogtoManagementEndpoint: "http://127.0.0.1:1",
			config.EnvLogtoM2MClientID:        "m2m",
			config.EnvLogtoM2MClientSecret:    "secret",
			config.EnvResendAPIKey:            "re_test",
			config.EnvEmailFrom:               "billing@delino.io",
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
	handler, err := newHandler(cfg, database, slog.New(slog.NewJSONHandler(io.Discard, nil)))
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

	req, _ := http.NewRequest(http.MethodOptions, srv.URL+accountsv1connect.BillingAccountServiceGetBillingAccountProcedure, nil)
	req.Header.Set("Origin", config.DefaultWebOrigin)
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	resp, err := http.DefaultClient.Do(req)
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
