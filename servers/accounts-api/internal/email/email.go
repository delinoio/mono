// SPDX-License-Identifier: FSL-1.1-ALv2

// Package email sends the billing emails of accounts-api through Resend
// (docs/servers-accounts-api-foundation.md, Emails). Sender is the
// abstraction; Resend sends through the Resend API and Fake records messages
// for tests.
package email

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/resend/resend-go/v3"
)

// Kind is the kind of a billing email. Logs and metrics carry the kind, never
// the address or the body.
type Kind string

const (
	// KindInvitation is the invitation to a billing account.
	KindInvitation Kind = "invitation"
)

// Message is one email.
type Message struct {
	Kind Kind
	// To is the recipient address. Never log it at the info level.
	To      string
	Subject string
	// Text and HTML are the plain text and HTML bodies. Never log them.
	Text string
	HTML string
	// IdempotencyKey makes the provider send the message once even when the
	// call is repeated, for example `invitation/<invitation_id>`.
	IdempotencyKey string
	// BillingAccountID is the billing account of the message, for the logs.
	BillingAccountID string
}

// Sender sends one email.
type Sender interface {
	Send(ctx context.Context, msg Message) error
}

// ResendConfig configures the Resend sender.
type ResendConfig struct {
	APIKey string
	// From is the sender address, such as `Delino <billing@delino.io>`.
	From string
	// BaseURL overrides the Resend API URL (tests); empty selects the
	// default.
	BaseURL string
	Client  *http.Client
	Logger  *slog.Logger
}

// Resend sends emails through the Resend API.
type Resend struct {
	client *resend.Client
	from   string
	logger *slog.Logger
}

// NewResend returns a Resend sender.
func NewResend(cfg ResendConfig) (*Resend, error) {
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	client := resend.NewCustomClient(cfg.Client, cfg.APIKey)
	if cfg.BaseURL != "" {
		if err := setBaseURL(client, cfg.BaseURL); err != nil {
			return nil, err
		}
	}
	return &Resend{client: client, from: cfg.From, logger: cfg.Logger}, nil
}

// Send implements Sender.
func (r *Resend) Send(ctx context.Context, msg Message) error {
	start := time.Now()
	req := &resend.SendEmailRequest{
		From:    r.from,
		To:      []string{msg.To},
		Subject: msg.Subject,
		Text:    msg.Text,
		Html:    msg.HTML,
		Tags:    []resend.Tag{{Name: "kind", Value: string(msg.Kind)}},
	}
	resp, err := r.client.Emails.SendWithOptions(ctx, req, &resend.SendEmailOptions{IdempotencyKey: msg.IdempotencyKey})
	if err != nil {
		r.logger.WarnContext(ctx, "billing_email_failed",
			slog.String("kind", string(msg.Kind)),
			slog.String("billing_account_id", msg.BillingAccountID),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			slog.String("error_kind", errorKind(err)))
		return fmt.Errorf("send %s email: %w", msg.Kind, errSendFailed)
	}
	r.logger.InfoContext(ctx, "billing_email_sent",
		slog.String("kind", string(msg.Kind)),
		slog.String("billing_account_id", msg.BillingAccountID),
		slog.String("resend_email_id", resp.Id),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()))
	return nil
}

// errSendFailed is the error that callers see; the provider error stays in
// the log.
var errSendFailed = errors.New("email delivery failed")

// errorKind classifies a Resend error for the log. The log never carries the
// provider message, because a validation message can quote the recipient
// address.
func errorKind(err error) string {
	var rateLimit *resend.RateLimitError
	var netErr *url.Error
	switch {
	case errors.As(err, &rateLimit):
		return "rate_limited"
	case errors.As(err, &netErr):
		return "network"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	default:
		// A rejected request or a provider failure; Resend returns its
		// message as plain error text.
		return "provider"
	}
}

// setBaseURL points the Resend client at another API URL (tests). The client
// resolves request paths against BaseURL, so the URL ends with a slash.
func setBaseURL(client *resend.Client, base string) error {
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("resend base url: %w", err)
	}
	client.BaseURL = u
	return nil
}

// Fake records messages instead of sending them.
type Fake struct {
	mu       sync.Mutex
	messages []Message
	err      error
	// errFor holds the per-recipient errors, keyed by the To address.
	errFor map[string]error
}

// NewFake returns an empty fake sender.
func NewFake() *Fake { return &Fake{} }

// Send implements Sender.
func (f *Fake) Send(_ context.Context, msg Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if err := f.errFor[msg.To]; err != nil {
		return err
	}
	f.messages = append(f.messages, msg)
	return nil
}

// SetErr makes every Send fail with err without recording the message; nil
// restores normal delivery.
func (f *Fake) SetErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// SetErrFor makes every Send to the address to fail with err without
// recording the message, while other recipients still succeed; nil restores
// normal delivery for that address. SetErr takes precedence.
func (f *Fake) SetErrFor(to string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.errFor, to)
		return
	}
	if f.errFor == nil {
		f.errFor = map[string]error{}
	}
	f.errFor[to] = err
}

// Messages returns a copy of the recorded messages in send order.
func (f *Fake) Messages() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Message(nil), f.messages...)
}
