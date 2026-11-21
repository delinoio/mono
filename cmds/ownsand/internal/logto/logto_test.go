// SPDX-License-Identifier: Apache-2.0

package logto

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPollTokenStates(t *testing.T) {
	for _, tc := range []struct {
		responses []string
		want      error
	}{
		{[]string{"slow_down", "authorization_pending"}, nil},
		{[]string{"access_denied"}, ErrAccessDenied},
		{[]string{"expired_token"}, ErrExpired},
	} {
		n := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if n < len(tc.responses) {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `{"error":%q}`, tc.responses[n])
				n++
				return
			}
			fmt.Fprint(w, `{"access_token":"a","refresh_token":"r","expires_in":60}`)
		}))
		cfg := Config{Endpoint: srv.URL, AppID: "app"}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		tok, err := cfg.PollToken(ctx, &DeviceAuthorization{DeviceCode: "d", ExpiresIn: 60}, time.Millisecond)
		cancel()
		srv.Close()
		if tc.want != nil {
			if !errors.Is(err, tc.want) {
				t.Errorf("%v: got %v", tc.responses, err)
			}
			continue
		}
		if err != nil || tok.RefreshToken != "r" {
			t.Errorf("%v: %v %v", tc.responses, tok, err)
		}
	}
}

func TestErrorsNeverContainTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant","error_description":"grant request is invalid"}`)
	}))
	defer srv.Close()
	_, err := Config{Endpoint: srv.URL, AppID: "app"}.Refresh(context.Background(), "refresh-secret")
	var oe *Error
	if !errors.As(err, &oe) || oe.Code != "invalid_grant" {
		t.Fatalf("err %v", err)
	}
	if got := err.Error(); got == "" || strings.Contains(got, "refresh-secret") {
		t.Fatalf("error %q", got)
	}
}

func TestValidate(t *testing.T) {
	if (Config{}).Validate() == nil || (Config{Endpoint: "ftp://x", AppID: "a"}).Validate() == nil {
		t.Fatal("invalid configuration accepted")
	}
	if err := (Config{Endpoint: "https://auth.example", AppID: "a"}).Validate(); err != nil {
		t.Fatal(err)
	}
}
