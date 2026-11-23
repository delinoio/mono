-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- The time that decides the UTC month of the monthly cap of an automatic
-- top-up. Owning contract: docs/servers-accounts-api-foundation.md (Payments
-- and credits, Storage). The change is additive and the column is nullable,
-- so the previous release keeps working while the new one starts. Apply this
-- migration before the new worker runs: the new queries read cap_time.

-- +goose Up

-- An automatic top-up is queued at one time and claimed for charging at a
-- later time; the claim checks the monthly cap in the month of the claim.
-- cap_time starts at the queue time and moves to the claim time in the
-- transaction that moves the top-up to charging, so the month-to-date total
-- of the cap counts the top-up in the UTC month in which it was checked and
-- charged. create_time stays unchanged as the history of the row. A row
-- without cap_time (every row that exists before this migration, and every
-- manual top-up) falls back to create_time.
ALTER TABLE top_ups ADD COLUMN cap_time timestamptz;

-- Serves the month-to-date total, which groups by the effective cap time.
CREATE INDEX top_ups_auto_by_cap_month ON top_ups (billing_account_id, (COALESCE(cap_time, create_time)))
    WHERE kind = 'auto';
