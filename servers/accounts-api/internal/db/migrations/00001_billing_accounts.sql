-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Billing accounts, members, invitations, organization links and billing
-- audit events. Owning contract: docs/servers-accounts-api-foundation.md
-- (Storage). Identifiers are UUID v7; user IDs are Logto `sub` values.

-- +goose Up

-- A billing account. Deletion is a soft delete (delete_time), so the
-- financial records and audit events that reference it stay for 7 years.
CREATE TABLE billing_accounts (
    billing_account_id uuid PRIMARY KEY,
    name text NOT NULL,
    -- Prepaid credit balance in micro-USD. A negative value is debt.
    balance_usd_micros bigint NOT NULL DEFAULT 0,
    create_time timestamptz NOT NULL,
    update_time timestamptz NOT NULL,
    delete_time timestamptz
);

-- Current memberships. Ended memberships are deleted; billing audit events
-- keep their history.
CREATE TABLE billing_account_members (
    billing_account_id uuid NOT NULL REFERENCES billing_accounts (billing_account_id),
    user_id text NOT NULL,
    role text NOT NULL CHECK (role IN ('owner', 'admin', 'viewer')),
    create_time timestamptz NOT NULL,
    PRIMARY KEY (billing_account_id, user_id)
);

-- A billing account has at most one owner. The application keeps exactly
-- one: it creates the owner with the account and moves the role only with an
-- ownership transfer.
CREATE UNIQUE INDEX billing_account_members_one_owner
    ON billing_account_members (billing_account_id) WHERE role = 'owner';
CREATE INDEX billing_account_members_by_user
    ON billing_account_members (user_id, billing_account_id);
CREATE INDEX billing_account_members_by_join
    ON billing_account_members (billing_account_id, create_time, user_id);

-- Email invitations. Only the SHA-256 hash of the invitation token is stored.
-- An invitation is pending while accept_time and revoke_time are null and
-- expire_time has not passed.
CREATE TABLE billing_account_invitations (
    invitation_id uuid PRIMARY KEY,
    billing_account_id uuid NOT NULL REFERENCES billing_accounts (billing_account_id),
    token_sha256 bytea NOT NULL UNIQUE CHECK (octet_length(token_sha256) = 32),
    -- The address as the inviter entered it, without surrounding white space.
    email text NOT NULL,
    -- The address with ASCII letters in lower case: the comparison key.
    email_key text NOT NULL,
    role text NOT NULL CHECK (role IN ('admin', 'viewer')),
    inviter_user_id text NOT NULL,
    create_time timestamptz NOT NULL,
    expire_time timestamptz NOT NULL,
    accept_time timestamptz,
    accepted_user_id text,
    revoke_time timestamptz,
    CHECK (accept_time IS NULL OR revoke_time IS NULL),
    CHECK ((accept_time IS NULL) = (accepted_user_id IS NULL))
);

CREATE INDEX billing_account_invitations_open
    ON billing_account_invitations (billing_account_id, invitation_id DESC)
    WHERE accept_time IS NULL AND revoke_time IS NULL;
CREATE INDEX billing_account_invitations_open_email
    ON billing_account_invitations (billing_account_id, email_key)
    WHERE accept_time IS NULL AND revoke_time IS NULL;

-- Links of service organizations to billing accounts, with their history.
-- A row with a null unlink_time is the current link; an organization of a
-- service has at most one.
CREATE TABLE organization_links (
    organization_link_id uuid PRIMARY KEY,
    -- Stable ServiceId identifier, such as `ownsand`.
    service text NOT NULL,
    organization_id text NOT NULL,
    billing_account_id uuid NOT NULL REFERENCES billing_accounts (billing_account_id),
    organization_name text NOT NULL DEFAULT '',
    link_time timestamptz NOT NULL,
    unlink_time timestamptz,
    CHECK (unlink_time IS NULL OR unlink_time >= link_time)
);

CREATE UNIQUE INDEX organization_links_current
    ON organization_links (service, organization_id) WHERE unlink_time IS NULL;
CREATE INDEX organization_links_current_by_account
    ON organization_links (billing_account_id) WHERE unlink_time IS NULL;
CREATE INDEX organization_links_history
    ON organization_links (service, organization_id, link_time);

-- Billing audit events, kept for 7 years. details holds kind-specific values
-- such as the old and the new role, never secrets or email addresses.
CREATE TABLE billing_audit_events (
    audit_event_id uuid PRIMARY KEY,
    billing_account_id uuid NOT NULL REFERENCES billing_accounts (billing_account_id),
    kind text NOT NULL,
    actor_user_id text NOT NULL,
    target_user_id text,
    invitation_id uuid REFERENCES billing_account_invitations (invitation_id),
    details jsonb NOT NULL DEFAULT '{}'::jsonb,
    create_time timestamptz NOT NULL
);

CREATE INDEX billing_audit_events_by_account
    ON billing_audit_events (billing_account_id, audit_event_id);
