-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Approved Paddle refunds and chargebacks that arrived before the credit
-- grant of their purchase. Owning contract:
-- docs/servers-accounts-api-foundation.md (Payments and credits, Paddle
-- webhook, Storage). The change is additive, so the previous release keeps
-- working while the new one starts.

-- +goose Up

-- One row per approved adjustment whose purchase has no credit grant yet.
-- The webhook that creates the grant of the purchase applies the row through
-- the refund accounting of credit_refunds and deletes it in the same
-- transaction. A row of a purchase that never creates a grant has no balance
-- effect. amount_usd_micros is the tax-exclusive subtotal of the adjustment;
-- the cap of the purchase applies when the row is applied.
CREATE TABLE pending_credit_adjustments (
    paddle_adjustment_id text PRIMARY KEY,
    paddle_transaction_id text NOT NULL,
    action text NOT NULL CHECK (action IN ('refund', 'chargeback')),
    amount_usd_micros bigint NOT NULL CHECK (amount_usd_micros >= 0),
    create_time timestamptz NOT NULL
);

CREATE INDEX pending_credit_adjustments_by_transaction
    ON pending_credit_adjustments (paddle_transaction_id, create_time, paddle_adjustment_id);
