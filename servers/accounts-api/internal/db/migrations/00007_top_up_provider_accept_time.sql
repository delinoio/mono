-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- The time Paddle accepted the charge of a top-up. Owning contract:
-- docs/servers-accounts-api-foundation.md (Payments and credits, Storage).
-- The change is additive and the column is nullable, so the previous release
-- keeps working while the new one starts. Apply this migration before the new
-- binary runs: the new queries read provider_accept_time.

-- +goose Up

-- Set in the transaction that records the accepted answer of Paddle for a
-- one-time charge (status charged). A timeout later replaces the status with
-- failed and the reason timed_out, which loses the fact that Paddle accepted
-- the charge; this column keeps it. A repeated manual top-up key then tells a
-- top-up that Paddle accepted (the payment webhook may still add the credit)
-- from one whose result stayed unknown. A row without the column set has no
-- evidence of acceptance: every row from before this migration, a charge with
-- an unknown result and a charge that Paddle rejected.
ALTER TABLE top_ups ADD COLUMN provider_accept_time timestamptz;
