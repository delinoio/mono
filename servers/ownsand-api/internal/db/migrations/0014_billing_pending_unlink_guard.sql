-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Explicit cleanup guard of a pending unlink. TransferOwnership and
-- DeleteOrganization turn a link attempt row into a plain pending unlink
-- (the attempt ID and the call deadline are cleared, see migration 0010)
-- whose first send must still wait until the LinkOrganization call of the
-- old attempt can no longer run. The wait used to live only in
-- next_attempt_time, which the retry worker honours but the synchronous
-- LinkBillingAccount and UnlinkBillingAccount paths do not, so a new
-- owner's request could send the unlink inside the window and delete the
-- recovery row while the old call could still commit a link. The owning
-- contract is the Billing links section of
-- docs/servers-ownsand-api-foundation.md.
--
-- unlink_not_before is the time before which no UnlinkOrganization is sent
-- for the row, by any path. It is NULL for a row without a guard and is a
-- plain timestamp that expires by itself; it needs no cleanup. The column
-- is nullable and has no constraint, so the previous release, which keeps
-- serving during a rolling deployment, reads and writes the rows with its
-- old column list. It does not know the guard: while both releases serve, a
-- takeover that the previous release performs leaves the column NULL (or a
-- stale value) and relies on next_attempt_time as before.

-- +goose Up

ALTER TABLE billing_pending_unlinks ADD COLUMN unlink_not_before timestamptz;
