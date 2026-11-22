// SPDX-License-Identifier: FSL-1.1-ALv2

package email_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/email"
)

func TestInvitationMessage(t *testing.T) {
	link := email.InvitationLink("https://accounts.delino.io/", "bai_abc-_123")
	if link != "https://accounts.delino.io/billing/invitations/accept#token=bai_abc-_123" {
		t.Fatalf("link = %q", link)
	}
	msg, err := email.InvitationMessage(email.Invitation{
		InvitationID:       "inv-1",
		BillingAccountID:   "ba-1",
		BillingAccountName: `<script>alert(1)</script> & Co`,
		Role:               "viewer",
		To:                 "b@example.com",
		Link:               link,
		ExpireTime:         time.Date(2026, 1, 8, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Kind != email.KindInvitation || msg.To != "b@example.com" || msg.IdempotencyKey != "invitation/inv-1" || msg.BillingAccountID != "ba-1" {
		t.Fatalf("message = %+v", msg)
	}
	if strings.Contains(msg.HTML, "<script>") {
		t.Fatal("the HTML body does not escape the account name")
	}
	for _, body := range []string{msg.Text, msg.HTML} {
		if !strings.Contains(body, "January 8, 2026 12:00 UTC") || !strings.Contains(body, "viewer") {
			t.Fatalf("body misses the expiry or the role: %s", body)
		}
	}
	if !strings.Contains(msg.Text, link) || !strings.Contains(msg.HTML, `href="https://accounts.delino.io/billing/invitations/accept#token=bai_abc-_123"`) {
		t.Fatalf("bodies miss the link:\n%s\n%s", msg.Text, msg.HTML)
	}
}

func TestResendSend(t *testing.T) {
	var got map[string]any
	var idem, authz string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emails" {
			http.NotFound(w, r)
			return
		}
		idem = r.Header.Get("Idempotency-Key")
		authz = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"email-1"}`))
	}))
	defer srv.Close()
	var logs bytes.Buffer
	sender, err := email.NewResend(email.ResendConfig{
		APIKey: "re_secret", From: "Delino <billing@delino.io>", BaseURL: srv.URL,
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	msg := email.Message{Kind: email.KindInvitation, To: "b@example.com", Subject: "S", Text: "secret-body", HTML: "<p>secret-body</p>", IdempotencyKey: "invitation/1", BillingAccountID: "ba-1"}
	if err := sender.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if idem != "invitation/1" || authz != "Bearer re_secret" {
		t.Fatalf("headers: idempotency %q, authorization %q", idem, authz)
	}
	if got["from"] != "Delino <billing@delino.io>" || got["subject"] != "S" || got["text"] != "secret-body" {
		t.Fatalf("request = %v", got)
	}
	for _, secret := range []string{"b@example.com", "secret-body", "re_secret"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("log contains %q: %s", secret, logs.String())
		}
	}
	if !strings.Contains(logs.String(), `"billing_email_sent"`) || !strings.Contains(logs.String(), `"kind":"invitation"`) {
		t.Fatalf("log = %s", logs.String())
	}
}

func TestResendFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"statusCode":422,"name":"validation_error","message":"Invalid to field: b@example.com"}`))
	}))
	defer srv.Close()
	var logs bytes.Buffer
	sender, err := email.NewResend(email.ResendConfig{APIKey: "k", From: "billing@delino.io", BaseURL: srv.URL, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	err = sender.Send(context.Background(), email.Message{Kind: email.KindInvitation, To: "b@example.com"})
	if err == nil || strings.Contains(err.Error(), "b@example.com") {
		t.Fatalf("Send() error = %v", err)
	}
	if strings.Contains(logs.String(), "b@example.com") || !strings.Contains(logs.String(), `"error_kind":"provider"`) {
		t.Fatalf("log = %s", logs.String())
	}
}

func TestResendErrorKind(t *testing.T) {
	const recipient = "b@example.com"
	respond := func(status int, body string, header map[string]string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			for k, v := range header {
				w.Header().Set(k, v)
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	}
	tests := []struct {
		name string
		want string
		// handler gets the channel that the test closes after the send, so a
		// blocking handler returns and srv.Close() does not hang.
		handler func(block chan struct{}) http.HandlerFunc
		// timeout is the http.Client.Timeout; zero keeps the default.
		timeout time.Duration
		// send builds the send context and returns a cleanup.
		ctx func() (context.Context, func())
		// closeServer closes the server before the send.
		closeServer bool
	}{
		{
			name: "canceled before the send",
			want: "canceled",
			handler: func(chan struct{}) http.HandlerFunc {
				return respond(http.StatusOK, `{"id":"e1"}`, nil)
			},
			ctx: func() (context.Context, func()) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, func() {}
			},
		},
		{
			name: "canceled in flight",
			want: "canceled",
			handler: func(block chan struct{}) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					select {
					case <-r.Context().Done():
					case <-block:
					}
				}
			},
			ctx: func() (context.Context, func()) {
				ctx, cancel := context.WithCancel(context.Background())
				timer := time.AfterFunc(50*time.Millisecond, cancel)
				return ctx, func() { timer.Stop(); cancel() }
			},
		},
		{
			name: "context deadline exceeded",
			want: "canceled",
			handler: func(block chan struct{}) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					select {
					case <-r.Context().Done():
					case <-block:
					}
				}
			},
			ctx: func() (context.Context, func()) {
				return context.WithTimeout(context.Background(), 50*time.Millisecond)
			},
		},
		{
			name: "client timeout with a live context",
			want: "network",
			handler: func(block chan struct{}) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					select {
					case <-r.Context().Done():
					case <-block:
					}
				}
			},
			timeout: 50 * time.Millisecond,
		},
		{
			name: "connection refused",
			want: "network",
			handler: func(chan struct{}) http.HandlerFunc {
				return respond(http.StatusOK, `{"id":"e1"}`, nil)
			},
			closeServer: true,
		},
		{
			name: "rate limited",
			want: "rate_limited",
			handler: func(chan struct{}) http.HandlerFunc {
				return respond(http.StatusTooManyRequests, `{"statusCode":429,"name":"rate_limit_exceeded","message":"Too many requests for b@example.com"}`,
					map[string]string{"ratelimit-limit": "2", "ratelimit-remaining": "0", "ratelimit-reset": "1", "retry-after": "1"})
			},
		},
		{
			name: "validation error",
			want: "provider",
			handler: func(chan struct{}) http.HandlerFunc {
				return respond(http.StatusUnprocessableEntity, `{"statusCode":422,"name":"validation_error","message":"Invalid to field: b@example.com"}`, nil)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			block := make(chan struct{})
			// close(block) is deferred after srv.Close, so it runs first.
			srv := httptest.NewServer(tc.handler(block))
			defer srv.Close()
			defer close(block)
			if tc.closeServer {
				srv.Close()
			}
			var logs bytes.Buffer
			cfg := email.ResendConfig{APIKey: "k", From: "billing@delino.io", BaseURL: srv.URL, Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
			if tc.timeout > 0 {
				cfg.Client = &http.Client{Timeout: tc.timeout}
			}
			sender, err := email.NewResend(cfg)
			if err != nil {
				t.Fatal(err)
			}
			ctx, done := context.Background(), func() {}
			if tc.ctx != nil {
				ctx, done = tc.ctx()
			}
			defer done()
			err = sender.Send(ctx, email.Message{Kind: email.KindInvitation, To: recipient})
			if err == nil || strings.Contains(err.Error(), recipient) {
				t.Fatalf("Send() error = %v", err)
			}
			if !strings.Contains(logs.String(), `"billing_email_failed"`) || !strings.Contains(logs.String(), `"error_kind":"`+tc.want+`"`) {
				t.Fatalf("want error_kind %q, log = %s", tc.want, logs.String())
			}
			if strings.Contains(logs.String(), recipient) {
				t.Fatalf("log contains the recipient: %s", logs.String())
			}
		})
	}
}

func TestFake(t *testing.T) {
	f := email.NewFake()
	if err := f.Send(context.Background(), email.Message{To: "a@example.com"}); err != nil {
		t.Fatal(err)
	}
	f.SetErr(errors.New("down"))
	if err := f.Send(context.Background(), email.Message{To: "b@example.com"}); err == nil {
		t.Fatal("Send() error = nil")
	}
	if got := f.Messages(); len(got) != 1 || got[0].To != "a@example.com" {
		t.Fatalf("Messages() = %+v", got)
	}
}
