-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Verification of exported snapshot parts. The control plane bills snapshot
-- storage from the part sizes that the agent reports, so it lists the export
-- attempt's prefix in R2 after every pre-signed upload URL of the attempt
-- expired and compares the objects with the manifest. The owning contract is
-- the Metering and Export recovery sections of
-- docs/servers-ownsand-api-foundation.md.
--
--   export_verified      true once the listing matched the manifest. A row
--                        that an earlier release marked EXPORTED, and a row
--                        that the previous release marks EXPORTED during a
--                        rolling deployment, keeps the default false, so the
--                        verifier checks it once.
--   export_verify_after  the earliest time the verifier may list the
--                        attempt's prefix. NULL for those rows: the verifier
--                        sets it to its first sight of the row plus the
--                        signed URL lifetime.
--
-- Both columns have a default or are nullable, so the previous release,
-- which keeps serving during a rolling deployment, still updates rows with
-- its old column list.

-- +goose Up

ALTER TABLE snapshots
    ADD COLUMN export_verified boolean NOT NULL DEFAULT false,
    ADD COLUMN export_verify_after timestamptz;

-- The verifier scans the EXPORTED rows that are not verified yet (export_state
-- 3 is SNAPSHOT_EXPORT_STATE_EXPORTED) in verification order.
CREATE INDEX snapshots_export_unverified_idx ON snapshots (export_verify_after NULLS FIRST, snapshot_id)
    WHERE export_state = 3 AND NOT export_verified;
