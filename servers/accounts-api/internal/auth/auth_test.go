// SPDX-License-Identifier: FSL-1.1-ALv2

package auth_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/auth"
	"github.com/delinoio/mono/servers/accounts-api/internal/auth/authtest"
)

func newValidator(iss *authtest.Issuer) *auth.LogtoValidator {
	return auth.NewLogtoValidator(auth.LogtoConfig{
		Issuer:   iss.URL,
		Audience: authtest.Audience,
		Logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
}

func reasonOf(t *testing.T, err error) auth.RejectReason {
	t.Helper()
	var invalid *auth.InvalidTokenError
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %v, want *InvalidTokenError", err)
	}
	return invalid.Reason
}

func TestValidateAcceptsUserAccessToken(t *testing.T) {
	iss := authtest.NewIssuer(t)
	tok, err := newValidator(iss).Validate(context.Background(), iss.UserToken(t, "user-1"))
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if tok.Subject != "user-1" || tok.MachineToMachine() || !tok.HasScope(auth.ScopeBilling) {
		t.Fatalf("token = %+v", tok)
	}
}

func TestValidateMachineToken(t *testing.T) {
	iss := authtest.NewIssuer(t)
	tok, err := newValidator(iss).Validate(context.Background(), iss.MachineToken(t, "app-1", auth.ScopeReportUsage))
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if !tok.MachineToMachine() {
		t.Fatal("MachineToMachine() = false")
	}
}

func TestValidateRejects(t *testing.T) {
	iss := authtest.NewIssuer(t)
	other := authtest.NewIssuer(t)
	v := newValidator(iss)
	for name, tc := range map[string]struct {
		raw  string
		want auth.RejectReason
	}{
		"garbage":        {"not-a-jwt", auth.RejectMalformed},
		"id token":       {iss.IDToken(t, "user-1"), auth.RejectTokenType},
		"wrong audience": {iss.Sign(t, authtest.Claims{Subject: "u", Audience: "https://other.example", Scopes: []string{auth.ScopeBilling}}), auth.RejectClaims},
		"expired":        {iss.Sign(t, authtest.Claims{Subject: "u", Expiry: time.Now().Add(-time.Hour)}), auth.RejectClaims},
		"no subject":     {iss.Sign(t, authtest.Claims{Scopes: []string{auth.ScopeBilling}}), auth.RejectClaims},
		"other key":      {other.Sign(t, authtest.Claims{Subject: "u"}), auth.RejectSignature},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := v.Validate(context.Background(), tc.raw)
			if got := reasonOf(t, err); got != tc.want {
				t.Fatalf("reason = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateRejectsWrongIssuer(t *testing.T) {
	iss := authtest.NewIssuer(t)
	v := auth.NewLogtoValidator(auth.LogtoConfig{
		Issuer:   "https://elsewhere.example/oidc",
		Audience: authtest.Audience,
		JWKSURL:  iss.JWKSURL,
		Logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	_, err := v.Validate(context.Background(), iss.UserToken(t, "u"))
	if got := reasonOf(t, err); got != auth.RejectClaims {
		t.Fatalf("reason = %q, want claims", got)
	}
}

func TestValidateUnreachableKeySet(t *testing.T) {
	v := auth.NewLogtoValidator(auth.LogtoConfig{
		Issuer:   "http://127.0.0.1:1/oidc",
		Audience: authtest.Audience,
		Client:   &http.Client{Timeout: time.Second},
		Logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	iss := authtest.NewIssuer(t)
	_, err := v.Validate(context.Background(), iss.UserToken(t, "u"))
	if got := reasonOf(t, err); got != auth.RejectUnknownKey {
		t.Fatalf("reason = %q, want unknown_key", got)
	}
}
