-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Prepaid credits: credit grants, the ledger, the Paddle subscription,
-- top-ups, processed Paddle events, refunds, credit settings and billing
-- notifications. Owning contract: docs/servers-accounts-api-foundation.md
-- (Payments and credits, Paddle webhook, Emails and Storage). Amounts are
-- micro-USD; Paddle IDs are stored unchanged.

-- +goose Up

-- A credit grant from one verified Paddle purchase. remaining_usd_micros is
-- the unused credit; usage consumes grants in expire_time order. Every credit
-- change keeps billing_accounts.balance_usd_micros equal to the sum of the
-- remaining credit while the balance is not negative; with a negative
-- balance (debt) every grant has nothing remaining.
CREATE TABLE credit_grants (
    credit_grant_id uuid PRIMARY KEY,
    billing_account_id uuid NOT NULL REFERENCES billing_accounts (billing_account_id),
    source text NOT NULL CHECK (source IN ('subscription_credit', 'manual_top_up', 'auto_top_up')),
    original_usd_micros bigint NOT NULL CHECK (original_usd_micros > 0),
    remaining_usd_micros bigint NOT NULL
        CHECK (remaining_usd_micros >= 0 AND remaining_usd_micros <= original_usd_micros),
    -- Credit removed by refunds and chargebacks of the purchase so far.
    refunded_usd_micros bigint NOT NULL DEFAULT 0
        CHECK (refunded_usd_micros >= 0 AND refunded_usd_micros <= original_usd_micros),
    -- One grant per Paddle transaction, so a repeated webhook adds nothing.
    paddle_transaction_id text NOT NULL UNIQUE,
    create_time timestamptz NOT NULL,
    expire_time timestamptz NOT NULL,
    -- When the email 7 days before the expiry was queued.
    expiry_notice_time timestamptz
);

CREATE INDEX credit_grants_usable
    ON credit_grants (billing_account_id, expire_time, credit_grant_id) WHERE remaining_usd_micros > 0;
CREATE INDEX credit_grants_due
    ON credit_grants (expire_time) WHERE remaining_usd_micros > 0;
CREATE INDEX credit_grants_by_account
    ON credit_grants (billing_account_id, create_time DESC, credit_grant_id DESC);

-- The append-only ledger. amount_usd_micros is signed: positive for credits,
-- negative or zero for usage, expiry and refunds.
CREATE TABLE ledger_entries (
    ledger_entry_id uuid PRIMARY KEY,
    billing_account_id uuid NOT NULL REFERENCES billing_accounts (billing_account_id),
    kind text NOT NULL CHECK (kind IN
        ('subscription_credit', 'manual_top_up', 'auto_top_up', 'usage', 'expiry', 'refund')),
    amount_usd_micros bigint NOT NULL,
    balance_after_usd_micros bigint NOT NULL,
    create_time timestamptz NOT NULL,
    paddle_transaction_id text,
    credit_grant_id uuid REFERENCES credit_grants (credit_grant_id),
    -- Usage entries only: the stable service and meter identifiers, the
    -- organization of the service, the window start and the quantity.
    service text,
    organization_id text,
    meter text,
    window_start timestamptz,
    quantity bigint NOT NULL DEFAULT 0,
    CHECK (CASE WHEN kind IN ('subscription_credit', 'manual_top_up', 'auto_top_up')
        THEN amount_usd_micros > 0 ELSE amount_usd_micros <= 0 END)
);

CREATE INDEX ledger_entries_by_account ON ledger_entries (billing_account_id, ledger_entry_id DESC);

-- The Paddle subscription of a billing account. A billing account without a
-- row has the status NONE.
CREATE TABLE subscriptions (
    billing_account_id uuid PRIMARY KEY REFERENCES billing_accounts (billing_account_id),
    status text NOT NULL CHECK (status IN ('pending', 'active', 'past_due', 'canceled')),
    -- The current Paddle subscription; null while pending.
    paddle_subscription_id text UNIQUE,
    paddle_customer_id text,
    -- The open checkout transaction of StartSubscription while pending.
    checkout_transaction_id text,
    cancel_at_period_end boolean NOT NULL DEFAULT false,
    current_period_end timestamptz,
    -- updated_at of the latest applied Paddle subscription entity, so an
    -- older event that arrives later does not overwrite a newer state.
    paddle_update_time timestamptz,
    create_time timestamptz NOT NULL,
    update_time timestamptz NOT NULL
);

-- Every checkout transaction and Paddle subscription that accounts-api
-- created or saw, with its billing account, so webhooks of earlier
-- subscriptions still find their billing account.
CREATE TABLE paddle_checkouts (
    paddle_transaction_id text PRIMARY KEY,
    billing_account_id uuid NOT NULL REFERENCES billing_accounts (billing_account_id),
    create_time timestamptz NOT NULL
);

CREATE TABLE paddle_subscriptions (
    paddle_subscription_id text PRIMARY KEY,
    billing_account_id uuid NOT NULL REFERENCES billing_accounts (billing_account_id),
    create_time timestamptz NOT NULL
);

-- Processed Paddle events. The event ID is recorded in the transaction that
-- applies the event, so a replayed event changes nothing.
CREATE TABLE paddle_events (
    paddle_event_id text PRIMARY KEY,
    event_type text NOT NULL,
    occurred_at timestamptz,
    billing_account_id uuid,
    result text NOT NULL,
    process_time timestamptz NOT NULL
);

-- Applied refunds and chargebacks, one per Paddle adjustment.
CREATE TABLE credit_refunds (
    paddle_adjustment_id text PRIMARY KEY,
    credit_grant_id uuid NOT NULL REFERENCES credit_grants (credit_grant_id),
    amount_usd_micros bigint NOT NULL CHECK (amount_usd_micros >= 0),
    ledger_entry_id uuid REFERENCES ledger_entries (ledger_entry_id),
    create_time timestamptz NOT NULL
);

-- Manual and automatic top-ups. Status flow: pending (automatic only, queued
-- by a deduction) -> charging (the Paddle call runs) -> charged (Paddle
-- accepted the charge) or unknown (the result of the call is unknown) ->
-- completed (the verified payment webhook added the credit) or failed.
CREATE TABLE top_ups (
    top_up_id uuid PRIMARY KEY,
    billing_account_id uuid NOT NULL REFERENCES billing_accounts (billing_account_id),
    kind text NOT NULL CHECK (kind IN ('manual', 'auto')),
    -- The caller's idempotency key of a manual top-up.
    idempotency_key uuid,
    actor_user_id text,
    amount_usd_micros bigint NOT NULL CHECK (amount_usd_micros > 0),
    status text NOT NULL CHECK (status IN ('pending', 'charging', 'charged', 'unknown', 'completed', 'failed')),
    failure_reason text,
    paddle_subscription_id text,
    paddle_transaction_id text UNIQUE,
    create_time timestamptz NOT NULL,
    update_time timestamptz NOT NULL,
    CHECK ((kind = 'manual') = (idempotency_key IS NOT NULL))
);

CREATE INDEX top_ups_idempotency ON top_ups (billing_account_id, idempotency_key, create_time)
    WHERE idempotency_key IS NOT NULL;
-- At most one automatic top-up is in flight for each billing account.
CREATE UNIQUE INDEX top_ups_auto_in_flight ON top_ups (billing_account_id)
    WHERE kind = 'auto' AND status IN ('pending', 'charging', 'charged', 'unknown');
CREATE INDEX top_ups_open ON top_ups (status, update_time)
    WHERE status IN ('pending', 'charging', 'charged', 'unknown');
CREATE INDEX top_ups_auto_by_month ON top_ups (billing_account_id, create_time) WHERE kind = 'auto';

-- Automatic top-up and low-balance alert settings. A billing account
-- without a row has automatic top-up off and the default alert threshold.
CREATE TABLE credit_settings (
    billing_account_id uuid PRIMARY KEY REFERENCES billing_accounts (billing_account_id),
    auto_top_up_enabled boolean NOT NULL DEFAULT false,
    auto_top_up_threshold_usd_micros bigint NOT NULL DEFAULT 0,
    auto_top_up_amount_usd_micros bigint NOT NULL DEFAULT 0,
    auto_top_up_monthly_cap_usd_micros bigint NOT NULL DEFAULT 0,
    auto_top_up_consecutive_failures integer NOT NULL DEFAULT 0,
    low_balance_threshold_usd_micros bigint NOT NULL,
    -- True after a low-balance email for the current period below the
    -- threshold; cleared when the balance reaches the threshold again.
    low_balance_alerted boolean NOT NULL DEFAULT false,
    -- Counts the periods below the threshold; part of the deduplication
    -- key of the low-balance email.
    low_balance_alert_count bigint NOT NULL DEFAULT 0,
    update_time timestamptz NOT NULL
);

-- Billing notification emails to the owners and admins. The deduplication
-- key makes each notification unique, so a retry never sends it twice.
-- details holds template values such as amounts and times, never email
-- addresses.
CREATE TABLE billing_notifications (
    notification_id uuid PRIMARY KEY,
    billing_account_id uuid NOT NULL REFERENCES billing_accounts (billing_account_id),
    kind text NOT NULL,
    dedup_key text NOT NULL UNIQUE,
    details jsonb NOT NULL DEFAULT '{}'::jsonb,
    create_time timestamptz NOT NULL,
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_time timestamptz NOT NULL,
    send_time timestamptz,
    abandon_time timestamptz
);

CREATE INDEX billing_notifications_due ON billing_notifications (next_attempt_time)
    WHERE send_time IS NULL AND abandon_time IS NULL;

-- Recipients that already received a notification.
CREATE TABLE billing_notification_deliveries (
    notification_id uuid NOT NULL REFERENCES billing_notifications (notification_id),
    user_id text NOT NULL,
    send_time timestamptz NOT NULL,
    PRIMARY KEY (notification_id, user_id)
);
