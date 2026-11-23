-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Indexes of the invitation volume limits. Owning contract:
-- docs/servers-accounts-api-foundation.md (Billing accounts and members,
-- Storage). The change only adds indexes, writes no rows and is compatible
-- with the previous release, which ignores them.

-- +goose Up

-- CreateInvitation counts the invitations that one inviter created, across all
-- billing accounts, in the last 24 hours (inviter_user_id, create_time), and
-- the invitations to one address from any inviter (email_key, create_time).
-- Both counts cover every state, so neither index is partial.
--
-- The migrations run in one transaction, so the indexes are built without
-- CONCURRENTLY, as in the earlier migrations. The build takes a SHARE lock on
-- billing_account_invitations: reads continue, and the writes of
-- CreateInvitation, AcceptInvitation and RevokeInvitation wait until the build
-- ends. The table holds only invitations, so the build is short.
CREATE INDEX billing_account_invitations_by_inviter_time
    ON billing_account_invitations (inviter_user_id, create_time);
CREATE INDEX billing_account_invitations_by_email_time
    ON billing_account_invitations (email_key, create_time);
