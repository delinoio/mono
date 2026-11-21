// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
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

// DeliverNotifications sends the due notifications. Each runs in its own
// transaction that locks the notification row (SKIP LOCKED, so several
// instances never deliver one notification at once) and records every
// recipient that received it, so a retry sends only to the others. The
// Resend idempotency key `<dedup key>/<user ID>` also covers a crash between
// the send and the commit.
func (s *Service) DeliverNotifications(ctx context.Context) error {
	for range 100 {
		var found bool
		err := s.db.InTx(ctx, func(tx pgx.Tx) error {
			now := s.timestamp()
			var n notification
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
			found = true
			n.Kind = NotificationKind(kind)
			if err := json.Unmarshal(details, &n.Details); err != nil {
				return err
			}
			return s.deliver(ctx, tx, n, now)
		})
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
	}
	return nil
}

// deliver sends one notification to every owner and admin that did not get
// it yet and records the result in tx.
func (s *Service) deliver(ctx context.Context, tx pgx.Tx, n notification, now time.Time) error {
	var name string
	var deleted bool
	if err := tx.QueryRow(ctx, `SELECT name, delete_time IS NOT NULL FROM billing_accounts WHERE billing_account_id = $1`,
		n.BillingAccountID).Scan(&name, &deleted); err != nil {
		return err
	}
	if deleted {
		_, err := tx.Exec(ctx, `UPDATE billing_notifications SET abandon_time = $2 WHERE notification_id = $1`, n.ID, now)
		return err
	}
	rows, err := tx.Query(ctx, `SELECT m.user_id FROM billing_account_members m
		WHERE m.billing_account_id = $1 AND m.role IN ('owner', 'admin')
			AND NOT EXISTS (SELECT 1 FROM billing_notification_deliveries d
				WHERE d.notification_id = $2 AND d.user_id = m.user_id)
		ORDER BY m.user_id`, n.BillingAccountID, n.ID)
	if err != nil {
		return err
	}
	recipients, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	lines := noticeLines(n)
	var failed error
	for _, userID := range recipients {
		user, err := s.directory.User(ctx, userID)
		if err != nil && !errors.Is(err, directory.ErrUserNotFound) {
			failed = fmt.Errorf("read recipient profile: %w", err)
			break
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
				s.metrics.Email(string(n.Kind), "failed")
				failed = err
				break
			}
			s.metrics.Email(string(n.Kind), "sent")
		}
		// A recipient without a primary email is recorded too, so it is not
		// read again on a retry.
		if _, err := tx.Exec(ctx, `INSERT INTO billing_notification_deliveries (notification_id, user_id, send_time)
			VALUES ($1, $2, $3)`, n.ID, userID, now); err != nil {
			return err
		}
	}
	if failed == nil {
		_, err := tx.Exec(ctx, `UPDATE billing_notifications SET send_time = $2, attempts = attempts + 1
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
		slog.Any("error", failed))
	_, err = tx.Exec(ctx, `UPDATE billing_notifications SET attempts = $2, next_attempt_time = $3, abandon_time = $4
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
	default:
		return nil
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
			_, err = s.afterDecrease(ctx, tx, acct, before, now, false)
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}
