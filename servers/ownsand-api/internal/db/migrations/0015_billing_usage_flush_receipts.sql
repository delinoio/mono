-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Receipts of usage flush batches. An instance adds its in-memory usage
-- counts to usage_counters in batches. Each batch has an ID (a UUID v7) that
-- the instance keeps until a flush of the batch succeeded, and the
-- transaction that applies a batch inserts the batch ID here first. When the
-- connection breaks during the commit, the instance cannot know whether the
-- batch committed and retries it under the same ID: a receipt that exists
-- already shows that the first attempt committed, so the retry adds nothing,
-- and no count is added twice or to a window that was sealed in between. The
-- owning contract is the Billing section of
-- docs/servers-ownsand-api-foundation.md.
--
-- A receipt only guards the retries of its own batch, so the janitor deletes
-- receipts after FlushReceiptRetention (7 days, see the contract). The table
-- is new and the counter write of the previous release, which keeps serving
-- during a rolling deployment, does not use it: that release neither reads
-- nor writes receipts and keeps its own retry behavior.

-- +goose Up

CREATE TABLE usage_flush_receipts (
    batch_id uuid PRIMARY KEY,
    create_time timestamptz NOT NULL
);
CREATE INDEX usage_flush_receipts_create_time_idx ON usage_flush_receipts (create_time);
