-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Keep billing_account_invitations_open for legacy ID-only page tokens.
-- Expiry stays in the query because it depends on the request time.

-- +goose Up

CREATE INDEX billing_account_invitations_open_by_time
    ON billing_account_invitations (billing_account_id, create_time DESC, invitation_id DESC)
    WHERE accept_time IS NULL AND revoke_time IS NULL;
