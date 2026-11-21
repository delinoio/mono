-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Usage reports and the billing status of linked organizations. Owning
-- contracts: docs/servers-accounts-api-foundation.md (Usage reports and
-- billing status, Storage) and docs/protos-delino-accounts-api-contract.md
-- (Usage reports, Billing status, Links). Every change is additive, so the
-- previous release keeps working while the new one starts.

-- +goose Up

-- Start of the uninterrupted non-payment of the billing account: the time
-- when the subscription stopped being ACTIVE or the balance fell to zero or
-- less. Null while the billing account is in good standing (or before its
-- standing was first evaluated).
ALTER TABLE billing_accounts ADD COLUMN nonpayment_since timestamptz;

-- The billing status of the current link. The values stay on a link row
-- after the unlink, but only current links (unlink_time is null) have a
-- status for the API. status_sequence is the position of the status in the
-- change sequence of the service.
ALTER TABLE organization_links
    ADD COLUMN billing_status text CHECK (billing_status IN ('allowed', 'blocked', 'suspended')),
    ADD COLUMN status_reason text
        CHECK (status_reason IN ('none', 'no_active_subscription', 'non_positive_balance')),
    ADD COLUMN blocked_since timestamptz,
    ADD COLUMN suspend_time timestamptz,
    ADD COLUMN status_sequence bigint CHECK (status_sequence > 0),
    ADD COLUMN status_update_time timestamptz,
    ADD CONSTRAINT organization_links_status_complete CHECK (
        (billing_status IS NULL AND status_reason IS NULL AND status_sequence IS NULL
            AND blocked_since IS NULL AND suspend_time IS NULL)
        OR (billing_status = 'allowed' AND status_reason = 'none' AND status_sequence IS NOT NULL
            AND blocked_since IS NULL AND suspend_time IS NULL)
        OR (billing_status IN ('blocked', 'suspended') AND status_reason <> 'none' AND status_sequence IS NOT NULL
            AND blocked_since IS NOT NULL AND suspend_time IS NOT NULL));

-- Sequences are unique within a service; the poll reads current links in
-- sequence order.
CREATE UNIQUE INDEX organization_links_status_sequence
    ON organization_links (service, status_sequence) WHERE status_sequence IS NOT NULL;
-- BLOCKED links whose suspension is due, for the worker.
CREATE INDEX organization_links_suspend_due
    ON organization_links (suspend_time) WHERE unlink_time IS NULL AND billing_status = 'blocked';
CREATE INDEX organization_links_current_by_link_time
    ON organization_links (billing_account_id, link_time, organization_link_id) WHERE unlink_time IS NULL;

-- The last status sequence of each service. A status change increments the
-- row and holds its lock until the transaction commits, so sequences become
-- visible in commit order and a poll cursor never skips a change.
CREATE TABLE billing_status_sequences (
    service text PRIMARY KEY,
    last_sequence bigint NOT NULL CHECK (last_sequence > 0)
);

-- Usage reports, kept for 7 years. One row per idempotency key (service,
-- organization, meter, window start). A report without an overlapping link
-- is unattributed: billing_account_id, organization_link_id and
-- ledger_entry_id are null and nothing was deducted.
CREATE TABLE usage_reports (
    usage_report_id uuid PRIMARY KEY,
    -- Stable ServiceId and Meter identifiers, such as `ownsand` and
    -- `ownsand.relay_egress`.
    service text NOT NULL,
    organization_id text NOT NULL,
    meter text NOT NULL,
    window_start timestamptz NOT NULL,
    quantity bigint NOT NULL CHECK (quantity >= 0),
    amount_usd_micros bigint NOT NULL CHECK (amount_usd_micros >= 0),
    billing_account_id uuid REFERENCES billing_accounts (billing_account_id),
    organization_link_id uuid REFERENCES organization_links (organization_link_id),
    -- The USAGE ledger entry; written in the transaction of the report.
    ledger_entry_id uuid REFERENCES ledger_entries (ledger_entry_id),
    create_time timestamptz NOT NULL,
    UNIQUE (service, organization_id, meter, window_start),
    CHECK ((billing_account_id IS NULL) = (organization_link_id IS NULL)),
    CHECK (billing_account_id IS NOT NULL OR ledger_entry_id IS NULL)
);

CREATE INDEX usage_reports_by_account
    ON usage_reports (billing_account_id, window_start) WHERE billing_account_id IS NOT NULL;
CREATE INDEX usage_reports_by_organization
    ON usage_reports (service, organization_id, window_start);
