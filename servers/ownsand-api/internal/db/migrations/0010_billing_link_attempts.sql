-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Durable record of a billing link attempt whose outcome is unknown.
-- LinkBillingAccount calls LinkOrganization on delino-accounts outside the
-- local transaction that records the link. A lost response or a failed
-- local commit used to leave a remote link without any local trace, so a
-- later unlink, ownership transfer or deletion could not remove it. The
-- owning contract is the Billing links section of
-- docs/servers-ownsand-api-foundation.md.
--
-- A link attempt reuses billing_pending_unlinks, whose rows already mean
-- that UnlinkOrganization must still be sent for the organization and that
-- the lease holder retries until delino-accounts confirms it:
--   link_attempt_id    NULL for a link that TransferOwnership or
--                      DeleteOrganization removed locally; the attempt ID of
--                      a LinkBillingAccount whose result is not confirmed
--                      locally. A completion of the attempt applies only
--                      while the row still carries its ID, so a stale
--                      completion never overwrites a newer operation.
--   link_call_deadline the time up to which the LinkOrganization call of the
--                      attempt can still be running; NULL once the call has
--                      returned. No unlink is sent before it, so the unlink
--                      cannot overtake the call.
-- billing_account_id holds the billing account of the attempt.
--
-- Both columns are nullable, so the previous release, which keeps serving
-- during a rolling deployment, reads and writes the rows unchanged and sends
-- the unlink of an attempt row like any pending unlink.

-- +goose Up

ALTER TABLE billing_pending_unlinks
    ADD COLUMN link_attempt_id uuid,
    ADD COLUMN link_call_deadline timestamptz,
    ADD CONSTRAINT billing_pending_unlinks_deadline_needs_attempt CHECK (link_call_deadline IS NULL OR link_attempt_id IS NOT NULL);
