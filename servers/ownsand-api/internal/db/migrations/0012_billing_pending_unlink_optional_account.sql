-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Optional billing account of a pending unlink. UnlinkOrganization on
-- delino-accounts needs only the service and the organization ID, so a
-- pending unlink does not need to know which billing account the
-- organization was linked to. Until now the control plane recorded a
-- pending unlink only when it knew the account (a cached link or a link
-- attempt record); a link that delino-accounts held without any local trace
-- (for example one that an earlier release created and lost) was never
-- removed by TransferOwnership or DeleteOrganization. They now record a
-- pending unlink for every organization, with no billing account when none
-- is known. The owning contract is the Billing links section of
-- docs/servers-ownsand-api-foundation.md.
--
-- billing_account_id is NULL for such a row; existing rows keep their
-- value. Only the NOT NULL constraint is dropped, so the previous release
-- keeps writing rows with its old column list. The previous release scans
-- the column into a non-nullable value, so while both releases serve during
-- a rolling deployment, it fails on a row without an account (the unlink of
-- that organization is retried by the new release, which reads it) and
-- never on a row that it or the new release wrote with an account.

-- +goose Up

ALTER TABLE billing_pending_unlinks ALTER COLUMN billing_account_id DROP NOT NULL;
