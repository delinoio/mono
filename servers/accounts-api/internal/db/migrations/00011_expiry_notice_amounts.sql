-- SPDX-License-Identifier: FSL-1.1-ALv2

-- +goose Up

-- New credit expiry notices use this schedule and set next_attempt_time to
-- infinity. Older workers cannot claim them and change the body for a key
-- that a newer worker may have used. Existing rows keep their old schedule.
ALTER TABLE billing_notifications
    ADD COLUMN expiry_next_attempt_time timestamptz,
    ADD CONSTRAINT billing_notifications_expiry_schedule
        CHECK (expiry_next_attempt_time IS NULL
            OR (kind = 'credit_expiry' AND next_attempt_time = 'infinity'));

CREATE INDEX billing_notifications_effective_due
    ON billing_notifications (COALESCE(expiry_next_attempt_time, next_attempt_time))
    WHERE send_time IS NULL AND abandon_time IS NULL;

-- A saved amount precedes a possible send. The existing delivery table still
-- records which recipients the worker has finished processing.
CREATE TABLE billing_notification_expiry_amounts (
    notification_id uuid NOT NULL REFERENCES billing_notifications (notification_id),
    user_id text NOT NULL,
    amount_usd_micros bigint NOT NULL CHECK (amount_usd_micros > 0),
    PRIMARY KEY (notification_id, user_id)
);
