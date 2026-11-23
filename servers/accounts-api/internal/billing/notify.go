// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/servers/accounts-api/internal/directory"
	"github.com/delinoio/mono/servers/accounts-api/internal/email"
)

// Notification delivery limits.
const (
	// notificationMaxAttempts is the number of delivery attempts before a
	// notification is abandoned.
	notificationMaxAttempts = 10
	// notificationMaxBackoff bounds the wait between attempts.
	notificationMaxBackoff = time.Hour
	// notificationLease is how long a claimed notification stays hidden from
	// the other workers. It exceeds workerRunTimeout, so a live pass keeps its
	// lease; a pass that crashed releases the notification when the lease
	// ends.
	notificationLease = 5 * time.Minute
	// notificationDeferDelay is the wait of a notification whose pass ended
	// early, before its next attempt.
	notificationDeferDelay = time.Minute
	// notificationSettleTimeout bounds each write that records the result of a
	// send or a pass after the pass context ended.
	notificationSettleTimeout = 10 * time.Second
)

// enqueue records a notification in tx. The deduplication key makes it
// unique: a repeated key records nothing, so a retried event never sends one
// notification twice. The worker delivers it after the transaction commits.
func (s *Service) enqueue(ctx context.Context, tx pgx.Tx, accountID string, kind NotificationKind, key string, details map[string]string, now time.Time) error {
	id, err := newID()
	if err != nil {
		return err
	}
	body, err := json.Marshal(details)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO billing_notifications (notification_id, billing_account_id, kind, dedup_key, details,
			create_time, next_attempt_time)
		VALUES ($1, $2, $3, $4, $5, $6, $6) ON CONFLICT (dedup_key) DO NOTHING`,
		id, accountID, string(kind), key, body, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		s.logger.InfoContext(ctx, "billing_notification_queued",
			slog.String("billing_account_id", accountID),
			slog.String("kind", string(kind)),
			slog.String("notification_id", id))
	}
	return nil
}

// notification is a queued notification.
type notification struct {
	ID               string
	BillingAccountID string
	Kind             NotificationKind
	DedupKey         string
	Details          map[string]string
	Attempts         int
}

// DeliverNotifications sends the due notifications. A notification is
// claimed in a short transaction that locks its row (SKIP LOCKED) and moves
// next_attempt_time forward by notificationLease. The moved time is the lease:
// no other instance picks the notification before it, so several instances
// never deliver one notification at once, and the lease needs no row lock
// during the sends. Each send commits its own recipient row right after it,
// so a pass that ends early (the worker deadline, a shutdown or a crash)
// keeps the progress, and a retry sends only to the other recipients. The
// Resend idempotency key `<dedup key>/<user ID>` also covers a crash between
// a send and its recipient row.
func (s *Service) DeliverNotifications(ctx context.Context) error {
	for range 100 {
		n, ok, err := s.claimNotification(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if err := s.deliver(ctx, n); err != nil {
			return err
		}
	}
	return nil
}

// claimNotification locks the next due notification and leases it. It
// reports false when none is due.
func (s *Service) claimNotification(ctx context.Context) (notification, bool, error) {
	var n notification
	found := false
	err := s.db.InTx(ctx, func(tx pgx.Tx) error {
		now := s.timestamp()
		var details []byte
		var kind string
		err := tx.QueryRow(ctx, `SELECT notification_id::text, billing_account_id::text, kind, dedup_key, details, attempts
			FROM billing_notifications
			WHERE send_time IS NULL AND abandon_time IS NULL AND next_attempt_time <= $1
			ORDER BY next_attempt_time LIMIT 1 FOR UPDATE SKIP LOCKED`, now).Scan(
			&n.ID, &n.BillingAccountID, &kind, &n.DedupKey, &details, &n.Attempts)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		n.Kind = NotificationKind(kind)
		if err := json.Unmarshal(details, &n.Details); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE billing_notifications SET next_attempt_time = $2 WHERE notification_id = $1`,
			n.ID, now.Add(notificationLease)); err != nil {
			return err
		}
		found = true
		return nil
	})
	return n, found, err
}

// settle runs a write that records the outcome of work that already
// happened: a send that went out, or the result of a whole pass. It does not
// follow the cancellation of ctx, because an expired worker deadline must not
// discard that work, but it is bounded by notificationSettleTimeout.
func (s *Service) settle(ctx context.Context, fn func(ctx context.Context) error) error {
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), notificationSettleTimeout)
	defer cancel()
	return fn(settleCtx)
}

// deliver sends the leased notification n to every owner and admin that did
// not get it yet and records the result.
func (s *Service) deliver(ctx context.Context, n notification) error {
	var name string
	var deleted bool
	if err := s.db.Pool.QueryRow(ctx, `SELECT name, delete_time IS NOT NULL FROM billing_accounts WHERE billing_account_id = $1`,
		n.BillingAccountID).Scan(&name, &deleted); err != nil {
		return s.interruptedOr(ctx, n, err)
	}
	if deleted {
		return s.settle(ctx, func(ctx context.Context) error {
			_, err := s.db.Pool.Exec(ctx, `UPDATE billing_notifications SET abandon_time = $2 WHERE notification_id = $1`,
				n.ID, s.timestamp())
			return err
		})
	}
	if stale, err := s.staleTopUpFailure(ctx, n); err != nil {
		return s.interruptedOr(ctx, n, err)
	} else if stale {
		return s.settle(ctx, func(ctx context.Context) error {
			if _, err := s.db.Pool.Exec(ctx, `UPDATE billing_notifications SET abandon_time = $2 WHERE notification_id = $1`,
				n.ID, s.timestamp()); err != nil {
				return err
			}
			s.logger.InfoContext(ctx, "billing_notification_suppressed",
				slog.String("billing_account_id", n.BillingAccountID),
				slog.String("kind", string(n.Kind)),
				slog.String("notification_id", n.ID),
				slog.String("top_up_id", topUpNotificationID(n)),
				slog.String("reason", "top_up_completed"))
			return nil
		})
	}
	rows, err := s.db.Pool.Query(ctx, `SELECT m.user_id FROM billing_account_members m
		WHERE m.billing_account_id = $1 AND m.role IN ('owner', 'admin')
			AND NOT EXISTS (SELECT 1 FROM billing_notification_deliveries d
				WHERE d.notification_id = $2 AND d.user_id = m.user_id)
		ORDER BY m.user_id`, n.BillingAccountID, n.ID)
	if err != nil {
		return s.interruptedOr(ctx, n, err)
	}
	recipients, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return s.interruptedOr(ctx, n, err)
	}
	lines := noticeLines(n)
	// A failure for one recipient does not stop the others: a recipient that
	// keeps failing must not keep later, independent recipients from getting
	// the email before the notification is abandoned. failed keeps the first
	// error for the retry bookkeeping and the log; every later error still
	// counts in the email metrics. A canceled or expired context stops the
	// loop, because every later call would fail the same way; the
	// notification is then deferred, not counted as a failed attempt.
	var failed error
	failedRecipients := 0
	for _, userID := range recipients {
		if ctx.Err() != nil {
			return s.deferNotification(ctx, n)
		}
		user, err := s.directory.User(ctx, userID)
		if err != nil && !errors.Is(err, directory.ErrUserNotFound) {
			if ctx.Err() != nil {
				return s.deferNotification(ctx, n)
			}
			failedRecipients++
			if failed == nil {
				failed = fmt.Errorf("read recipient profile: %w", err)
			}
			continue
		}
		if user.PrimaryEmail != "" {
			msg, err := email.NoticeMessage(email.Notice{
				Kind:               email.Kind(n.Kind),
				BillingAccountID:   n.BillingAccountID,
				BillingAccountName: name,
				To:                 user.PrimaryEmail,
				Link:               email.BillingAccountPath(s.webOrigin, n.BillingAccountID),
				Lines:              lines,
				IdempotencyKey:     n.DedupKey + "/" + userID,
			})
			if err != nil {
				return err
			}
			if err := s.email.Send(ctx, msg); err != nil {
				if ctx.Err() != nil {
					// The pass ended while the send was in flight, so the
					// outcome is unknown. The next attempt sends again with
					// the same idempotency key.
					return s.deferNotification(ctx, n)
				}
				s.metrics.Email(string(n.Kind), "failed")
				failedRecipients++
				if failed == nil {
					failed = err
				}
				continue
			}
			s.metrics.Email(string(n.Kind), "sent")
		}
		// Record the recipient at once, so a pass that ends early keeps it.
		// A recipient without a primary email is recorded too, so it is not
		// read again on a retry.
		if err := s.settle(ctx, func(ctx context.Context) error {
			_, err := s.db.Pool.Exec(ctx, `INSERT INTO billing_notification_deliveries (notification_id, user_id, send_time)
				VALUES ($1, $2, $3) ON CONFLICT (notification_id, user_id) DO NOTHING`, n.ID, userID, s.timestamp())
			return err
		}); err != nil {
			return err
		}
	}
	return s.settle(ctx, func(ctx context.Context) error {
		return s.finish(ctx, n, failed, failedRecipients)
	})
}

// topUpNotificationPrefix is the deduplication key prefix of a top-up failure
// notification; the top-up ID follows it.
const topUpNotificationPrefix = "payment_failed/top_up/"

// topUpNotificationID returns the ID of the top-up that a payment-failed
// top-up notification reports, or "" for any other notification. A
// notification queued before the details carried the ID has it only in its
// deduplication key.
//
// Workaround scope: the deduplication key fallback exists for rows that are
// already queued. Remove it once no queued row without notifyDetailTopUpID
// can exist (all such rows are sent or abandoned after a release that writes
// the detail).
func topUpNotificationID(n notification) string {
	if n.Kind != NotifyPaymentFailed || n.Details[notifyDetailPaymentScope] != notifyScopeTopUp {
		return ""
	}
	if id := n.Details[notifyDetailTopUpID]; id != "" {
		return id
	}
	return strings.TrimPrefix(n.DedupKey, topUpNotificationPrefix)
}

// staleTopUpFailure reports whether n is a payment-failed notification of a
// top-up that completed after the failure was queued, which a late verified
// webhook can do. Such a notice would tell the owners that no credit was
// added when it was. The read does not lock the top-up, so a completion that
// commits after this read but before the send still lets the email go out;
// closing that window would need the send inside the top-up transaction.
func (s *Service) staleTopUpFailure(ctx context.Context, n notification) (bool, error) {
	id := topUpNotificationID(n)
	if id == "" {
		return false, nil
	}
	var completed bool
	err := s.db.Pool.QueryRow(ctx, `SELECT status = 'completed' FROM top_ups WHERE top_up_id::text = $1`, id).Scan(&completed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return completed, err
}

// interruptedOr defers n when err is the effect of a pass context that ended,
// and returns err otherwise.
func (s *Service) interruptedOr(ctx context.Context, n notification, err error) error {
	if ctx.Err() != nil {
		return s.deferNotification(ctx, n)
	}
	return err
}

// deferNotification ends the pass of an interrupted notification. The
// recipients that are not recorded stay pending, the attempt count stays as
// it is, and the notification waits notificationDeferDelay, so that a
// notification that became due meanwhile runs first and the same one does
// not block the queue. It returns the error of ctx.
func (s *Service) deferNotification(ctx context.Context, n notification) error {
	err := s.settle(ctx, func(ctx context.Context) error {
		_, err := s.db.Pool.Exec(ctx, `UPDATE billing_notifications SET next_attempt_time = $2 WHERE notification_id = $1`,
			n.ID, s.timestamp().Add(notificationDeferDelay))
		return err
	})
	s.logger.WarnContext(ctx, "billing_notification_deferred",
		slog.String("billing_account_id", n.BillingAccountID),
		slog.String("kind", string(n.Kind)),
		slog.String("notification_id", n.ID),
		slog.Any("error", err))
	if err != nil {
		return err
	}
	return ctx.Err()
}

// finish records the result of a pass that reached every recipient: sent, or
// failed for failedRecipients of them with the first error failed.
func (s *Service) finish(ctx context.Context, n notification, failed error, failedRecipients int) error {
	now := s.timestamp()
	if failed == nil {
		_, err := s.db.Pool.Exec(ctx, `UPDATE billing_notifications SET send_time = $2, attempts = attempts + 1
			WHERE notification_id = $1`, n.ID, now)
		if err == nil {
			s.logger.InfoContext(ctx, "billing_notification_sent",
				slog.String("billing_account_id", n.BillingAccountID),
				slog.String("kind", string(n.Kind)),
				slog.String("notification_id", n.ID))
		}
		return err
	}
	attempts := n.Attempts + 1
	backoff := min(time.Minute<<min(attempts-1, 10), notificationMaxBackoff)
	var abandon *time.Time
	if attempts >= notificationMaxAttempts {
		abandon = &now
	}
	s.logger.WarnContext(ctx, "billing_notification_failed",
		slog.String("billing_account_id", n.BillingAccountID),
		slog.String("kind", string(n.Kind)),
		slog.String("notification_id", n.ID),
		slog.Int("attempts", attempts),
		slog.Bool("abandoned", abandon != nil),
		slog.Int("failed_recipients", failedRecipients),
		slog.Any("error", failed))
	_, err := s.db.Pool.Exec(ctx, `UPDATE billing_notifications SET attempts = $2, next_attempt_time = $3, abandon_time = $4
		WHERE notification_id = $1`, n.ID, attempts, now.Add(backoff), abandon)
	return err
}

// noticeLines renders the body sentences of a notification from its
// details. Amounts are rounded to cents for display only.
func noticeLines(n notification) []string {
	amount := func(key string) string {
		v, err := strconv.ParseInt(n.Details[key], 10, 64)
		if err != nil {
			return "an unknown amount"
		}
		return FormatUSD(v)
	}
	switch n.Kind {
	case NotifyPaymentFailed:
		if n.Details[notifyDetailPaymentScope] == notifyScopeSubscription {
			return []string{
				"The payment of your $10 monthly Delino subscription failed.",
				"Paddle retries the payment. Update the payment method of the subscription to keep the subscription active.",
			}
		}
		what := "A top-up"
		if n.Details[notifyDetailTopUpKind] == string(TopUpAuto) {
			what = "An automatic top-up"
		}
		return []string{
			fmt.Sprintf("%s of %s failed, and no credit was added.", what, amount(notifyDetailAmount)),
			"Check the payment method of the subscription.",
		}
	case NotifyAutoTopUpDisabled:
		return []string{
			"Automatic top-up failed 2 times in a row and is now turned off.",
			"Check the payment method of the subscription and turn automatic top-up on again.",
		}
	case NotifyLowBalance:
		return []string{
			fmt.Sprintf("The credit balance is %s, below the alert threshold of %s.", amount(notifyDetailBalance), amount(notifyDetailThreshold)),
			"Add credit with a top-up so that your services keep running.",
		}
	case NotifyCreditExpiry:
		expires := n.Details[notifyDetailExpireTime]
		if t, err := time.Parse(time.RFC3339Nano, expires); err == nil {
			expires = t.UTC().Format("January 2, 2006 15:04 UTC")
		}
		return []string{
			fmt.Sprintf("%s of credit expires on %s.", amount(notifyDetailAmount), expires),
			"Unused credit leaves the balance when it expires.",
		}
	case NotifyOrganizationBlocked, NotifyOrganizationSuspended:
		return organizationLines(n)
	default:
		return nil
	}
}

// organizationLines renders the blocked and suspended emails. The
// organization name comes from the calling service; html/template escapes
// it in the HTML body.
func organizationLines(n notification) []string {
	org := fmt.Sprintf("The %s organization", ServiceDisplayName(n.Details[notifyDetailService]))
	if name := n.Details[notifyDetailOrganizationName]; name != "" {
		org += fmt.Sprintf(" %q", name)
	}
	org += fmt.Sprintf(" (%s)", n.Details[notifyDetailOrganizationID])
	cause := "the billing account has no active subscription"
	if n.Details[notifyDetailReason] == string(ReasonNonPositiveBalance) {
		cause = "the credit balance of the billing account is $0.00 or less"
	}
	suspend := n.Details[notifyDetailSuspendTime]
	if t, err := time.Parse(time.RFC3339Nano, suspend); err == nil {
		suspend = t.UTC().Format("January 2, 2006 15:04 UTC")
	}
	if n.Kind == NotifyOrganizationSuspended {
		return []string{
			fmt.Sprintf("%s is suspended because the billing account was not paid for 3 days: %s.", org, cause),
			"Start the subscription and add credit so that the organization can be used again.",
		}
	}
	return []string{
		fmt.Sprintf("%s is blocked because %s.", org, cause),
		fmt.Sprintf("It will be suspended on %s unless the subscription is active and the balance is above $0.00 before then.", suspend),
	}
}

// QueueExpiryNotices queues the email for grants that expire within 7 days
// and still have credit.
func (s *Service) QueueExpiryNotices(ctx context.Context) error {
	now := s.timestamp()
	rows, err := s.db.Pool.Query(ctx, `SELECT credit_grant_id::text, billing_account_id::text FROM credit_grants
		WHERE remaining_usd_micros > 0 AND expiry_notice_time IS NULL AND expire_time > $1 AND expire_time <= $2
		ORDER BY expire_time LIMIT 100`, now, now.Add(ExpiryNoticeLead))
	if err != nil {
		return err
	}
	type ref struct{ GrantID, AccountID string }
	refs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[ref])
	if err != nil {
		return err
	}
	for _, r := range refs {
		err := s.db.InTx(ctx, func(tx pgx.Tx) error {
			acct, err := lockAccount(ctx, tx, r.AccountID)
			if err != nil {
				return err
			}
			var remaining int64
			var expire time.Time
			var noticed *time.Time
			if err := tx.QueryRow(ctx, `SELECT remaining_usd_micros, expire_time, expiry_notice_time FROM credit_grants
				WHERE credit_grant_id = $1 FOR UPDATE`, r.GrantID).Scan(&remaining, &expire, &noticed); err != nil {
				return err
			}
			if noticed != nil {
				return nil
			}
			if _, err := tx.Exec(ctx, `UPDATE credit_grants SET expiry_notice_time = $2 WHERE credit_grant_id = $1`,
				r.GrantID, now); err != nil {
				return err
			}
			if acct.Deleted || remaining <= 0 {
				return nil
			}
			return s.enqueue(ctx, tx, acct.ID, NotifyCreditExpiry, "credit_expiry/"+r.GrantID, map[string]string{
				notifyDetailAmount:     fmt.Sprint(remaining),
				notifyDetailExpireTime: expire.UTC().Format(time.RFC3339Nano),
			}, now)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// ExpireGrants removes the credit of expired grants with EXPIRY entries.
func (s *Service) ExpireGrants(ctx context.Context) error {
	now := s.timestamp()
	rows, err := s.db.Pool.Query(ctx, `SELECT DISTINCT billing_account_id::text FROM credit_grants
		WHERE remaining_usd_micros > 0 AND expire_time <= $1 LIMIT 100`, now)
	if err != nil {
		return err
	}
	accounts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, accountID := range accounts {
		err := s.db.InTx(ctx, func(tx pgx.Tx) error {
			now := s.timestamp()
			acct, err := lockAccount(ctx, tx, accountID)
			if err != nil {
				return err
			}
			before := acct.Balance
			if err := s.expireDue(ctx, tx, acct, now); err != nil {
				return err
			}
			if _, err := s.afterDecrease(ctx, tx, acct, before, now, false); err != nil {
				return err
			}
			return s.syncStatuses(ctx, tx, acct.ID, now)
		})
		if err != nil {
			return err
		}
	}
	return nil
}
