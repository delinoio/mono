-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Billing usage metering: the shared usage counters, the usage outbox and
-- the bookkeeping of sealing and R2 storage sampling. The owning contract
-- is the Billing section of docs/servers-ownsand-api-foundation.md.
--
-- Counter and outbox rows have no foreign key to the organization: an
-- organization deletion keeps them until they are reported, and its debt
-- stays on the billing account. meter holds the delino.ownsand.v1
-- BillingMeter value; window_start is the start of an aligned 5-minute UTC
-- window.

-- Usage of the open (not yet sealed) windows. Every instance adds its
-- in-memory counts here at least every 30 seconds and at a graceful
-- shutdown.
CREATE TABLE usage_counters (
    organization_id uuid NOT NULL,
    meter smallint NOT NULL,
    window_start timestamptz NOT NULL,
    quantity bigint NOT NULL CHECK (quantity >= 0),
    PRIMARY KEY (organization_id, meter, window_start)
);
CREATE INDEX usage_counters_window_idx ON usage_counters (window_start);

-- Sealed windows. The quantity and the amount of a row never change after
-- sealing, so a retry never reports a window with another amount.
CREATE TABLE usage_outbox (
    organization_id uuid NOT NULL,
    meter smallint NOT NULL,
    window_start timestamptz NOT NULL,
    quantity bigint NOT NULL CHECK (quantity > 0),
    amount_usd_micros bigint NOT NULL CHECK (amount_usd_micros >= 0),
    seal_time timestamptz NOT NULL,
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_time timestamptz NOT NULL,
    report_time timestamptz,
    PRIMARY KEY (organization_id, meter, window_start)
);
CREATE INDEX usage_outbox_unreported_idx ON usage_outbox (next_attempt_time) WHERE report_time IS NULL;
CREATE INDEX usage_outbox_reported_idx ON usage_outbox (report_time) WHERE report_time IS NOT NULL;

-- One row: every window that starts before sealed_before is sealed. A seal
-- holds the row lock (FOR UPDATE) while it moves counters to the outbox; a
-- counter write holds it in share mode and moves a count of a sealed window
-- to the oldest open window, so no count is lost or added to a sealed
-- window.
CREATE TABLE usage_seal_state (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    sealed_before timestamptz NOT NULL
);
INSERT INTO usage_seal_state (singleton, sealed_before) VALUES (true, '1970-01-01T00:00:00Z');

-- Windows whose R2 storage has been sampled, so each window is sampled once
-- even across lease handovers.
CREATE TABLE usage_storage_samples (
    window_start timestamptz PRIMARY KEY,
    sample_time timestamptz NOT NULL
);
