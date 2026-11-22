// SPDX-License-Identifier: FSL-1.1-ALv2

// Package billing holds the prepaid credit logic of accounts-api
// (docs/servers-accounts-api-foundation.md, Payments and credits, Paddle
// webhook and Emails): the credit ledger with FIFO grants and debt, the
// Paddle subscription, manual and automatic top-ups, the Paddle webhook,
// billing notifications and the background worker.
//
// Concurrency: every change of the credit of a billing account locks the
// billing account row first (SELECT ... FOR UPDATE), so changes of one
// billing account serialize and billing_accounts.balance_usd_micros stays
// equal to the ledger in the same transaction.
package billing

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/uuidv7"
	"github.com/delinoio/mono/servers/accounts-api/internal/db"
	"github.com/delinoio/mono/servers/accounts-api/internal/directory"
	"github.com/delinoio/mono/servers/accounts-api/internal/email"
	"github.com/delinoio/mono/servers/accounts-api/internal/metrics"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
)

// Config holds the dependencies and the Paddle catalog IDs.
type Config struct {
	DB        *db.DB
	Paddle    *paddle.Client
	Directory directory.Directory
	Email     email.Sender
	Metrics   *metrics.Metrics
	Logger    *slog.Logger
	// Now is the business clock; nil selects time.Now.
	Now func() time.Time
	// WebhookSecret is the secret of the Paddle notification destination.
	// Secret: never log it.
	WebhookSecret string
	// SubscriptionPriceID is the Paddle price of the $10/month subscription.
	SubscriptionPriceID string
	// TopUpProductID is the Paddle product of top-up charges.
	TopUpProductID string
	// WebOrigin is the origin of the web app, for links in emails.
	WebOrigin string
}

// Service is the billing logic. It is safe for concurrent use.
type Service struct {
	db            *db.DB
	paddle        *paddle.Client
	directory     directory.Directory
	email         email.Sender
	metrics       *metrics.Metrics
	logger        *slog.Logger
	now           func() time.Time
	webhookSecret string
	priceID       string
	productID     string
	webOrigin     string

	// paddleCallTimeout and outcomeTimeout are the deadlines of the Paddle
	// call of a top-up and of the transaction that records its outcome. They
	// are fields so that tests can shorten them.
	paddleCallTimeout time.Duration
	outcomeTimeout    time.Duration

	// wake asks the worker to charge queued automatic top-ups at once.
	wake chan struct{}
}

// New returns the billing service.
func New(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Service{
		db:            cfg.DB,
		paddle:        cfg.Paddle,
		directory:     cfg.Directory,
		email:         cfg.Email,
		metrics:       cfg.Metrics,
		logger:        cfg.Logger,
		now:           cfg.Now,
		webhookSecret: cfg.WebhookSecret,
		priceID:       cfg.SubscriptionPriceID,
		productID:     cfg.TopUpProductID,
		webOrigin:     cfg.WebOrigin,

		paddleCallTimeout: paddleCallTimeout,
		outcomeTimeout:    topUpOutcomeTimeout,
		wake:              make(chan struct{}, 1),
	}
}

// Wake asks the worker to process queued automatic top-ups now. Callers of
// Deduct call it after their transaction committed with AutoTopUpQueued.
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// timestamp returns the current business time at the precision of Postgres.
func (s *Service) timestamp() time.Time {
	return s.now().UTC().Truncate(time.Microsecond)
}

func newID() (string, error) { return uuidv7.NewString() }

// Connect errors returned to RPC callers. Messages never contain payment
// data.
func failedPrecondition(msg string) error {
	return connect.NewError(connect.CodeFailedPrecondition, errors.New(msg))
}

func invalidArgument(msg string) error {
	return connect.NewError(connect.CodeInvalidArgument, errors.New(msg))
}

func alreadyExists(msg string) error {
	return connect.NewError(connect.CodeAlreadyExists, errors.New(msg))
}

func unavailable(msg string) error {
	return connect.NewError(connect.CodeUnavailable, errors.New(msg))
}

func notFound(msg string) error {
	return connect.NewError(connect.CodeNotFound, errors.New(msg))
}

// errAccountMissing is returned when a billing account row does not exist.
var errAccountMissing = errors.New("billing account does not exist")

// isContextErr reports whether err comes from a canceled context.
func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
