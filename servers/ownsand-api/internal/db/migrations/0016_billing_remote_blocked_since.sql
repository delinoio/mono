-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- The block start that delino-accounts last reported for a linked
-- organization. The cache keeps the earlier of its own block start and the
-- reported one while the organization stays in non-payment, so a link never
-- restarts the 3-day suspension period. The cache could not tell that rule
-- apart from a new non-payment interval: when an organization recovers and
-- is blocked again between two polls, only the newest status (BLOCKED with
-- the new, later start) reaches the control plane, and the earlier local
-- start kept the new interval from restarting the period. Comparing the new
-- reported start with the one that was last applied tells the two cases
-- apart: a later reported start is a new interval, an equal or earlier one
-- is the same interval reported again. The owning contract is the Billing
-- status cache section of docs/servers-ownsand-api-foundation.md.
--
-- The column is NULL for an organization without a link, in ALLOWED and
-- after an unlink, and for every row that exists when the migration runs, so
-- the behavior of an existing row is unchanged until the next status from
-- delino-accounts. The column is nullable and has no constraint, so the
-- previous release, which keeps serving during a rolling deployment, reads
-- and writes the rows with its old column list; it does not maintain the
-- column, and a value that it leaves behind at most makes the cache treat a
-- later reported start as a new interval once.

-- +goose Up

ALTER TABLE organization_billing ADD COLUMN remote_blocked_since timestamptz;
