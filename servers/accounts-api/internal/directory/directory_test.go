// SPDX-License-Identifier: FSL-1.1-ALv2

package directory_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/delinoio/mono/servers/accounts-api/internal/directory"
)

func TestLogtoUser(t *testing.T) {
	var tokens, rejectFirst atomic.Int32
	rejectFirst.Store(1)
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/oidc/token":
			id, secret, ok := r.BasicAuth()
			if !ok || id != "m2m" || secret != "s3cret" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = r.ParseForm()
			if r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("resource") != srvURL+"/api" || r.Form.Get("scope") != "all" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			n := tokens.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok" + string(rune('0'+n)), "expires_in": 3600})
		case r.Method == http.MethodGet && r.URL.Path == "/api/users/u1":
			if rejectFirst.CompareAndSwap(1, 0) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.Header.Get("Authorization") != "Bearer tok2" && r.Header.Get("Authorization") != "Bearer tok1" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "u1", "primaryEmail": "A@Example.com", "name": "Ann", "username": nil})
		case r.Method == http.MethodGet && r.URL.Path == "/api/users/u2":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "u2", "primaryEmail": nil, "name": nil})
		case r.Method == http.MethodGet && r.URL.Path == "/api/users/broken":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL
	l := directory.NewLogto(directory.LogtoConfig{
		Endpoint: srv.URL + "/", ClientID: "m2m", ClientSecret: "s3cret",
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	ctx := context.Background()
	user, err := l.User(ctx, "u1")
	if err != nil {
		t.Fatalf("User(u1) error = %v", err)
	}
	if user.PrimaryEmail != "A@Example.com" || user.Name != "Ann" {
		t.Fatalf("User(u1) = %+v", user)
	}
	if tokens.Load() != 2 {
		t.Fatalf("token requests = %d, want 2 (one renewal after 401)", tokens.Load())
	}
	user, err = l.User(ctx, "u2")
	if err != nil || user != (directory.User{}) {
		t.Fatalf("User(u2) = %+v, %v", user, err)
	}
	if tokens.Load() != 2 {
		t.Fatalf("the cached token was not reused: %d requests", tokens.Load())
	}
	if _, err := l.User(ctx, "missing"); !errors.Is(err, directory.ErrUserNotFound) {
		t.Fatalf("User(missing) error = %v, want ErrUserNotFound", err)
	}
	if _, err := l.User(ctx, "broken"); err == nil || errors.Is(err, directory.ErrUserNotFound) {
		t.Fatalf("User(broken) error = %v, want an unavailable error", err)
	}
}

func TestLogtoTokenFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	l := directory.NewLogto(directory.LogtoConfig{Endpoint: srv.URL, ClientID: "m2m", ClientSecret: "wrong"})
	if _, err := l.User(context.Background(), "u1"); err == nil || errors.Is(err, directory.ErrUserNotFound) {
		t.Fatalf("User() error = %v, want a token error", err)
	}
}
