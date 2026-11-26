-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Core tables of sandrun-api: organizations with their settings, memberships,
-- invitations, personal API keys, audit events and the instance lease.
-- Enum columns store the protobuf enum numbers of delino.sandrun.v1.
-- The owning contract is the Storage section of
-- docs/servers-sandrun-api-foundation.md.

-- +goose Up
CREATE TABLE organizations (
    organization_id uuid PRIMARY KEY,
    name text NOT NULL,
    -- NULL means no limit (OrganizationSettings.max_dev_sessions_per_user).
    max_dev_sessions_per_user integer CHECK (max_dev_sessions_per_user > 0),
    create_time timestamptz NOT NULL
);

CREATE TABLE memberships (
    membership_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    -- Logto `sub`.
    user_id text NOT NULL,
    -- sandrunv1.Role: 1 owner, 2 admin, 3 member.
    role smallint NOT NULL CHECK (role IN (1, 2, 3)),
    join_time timestamptz NOT NULL,
    UNIQUE (organization_id, user_id)
);
-- An organization has exactly one owner; this index rejects a second one.
CREATE UNIQUE INDEX memberships_one_owner_idx ON memberships (organization_id) WHERE role = 1;
CREATE INDEX memberships_user_idx ON memberships (user_id, organization_id);

CREATE TABLE invitations (
    invitation_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    -- sandrunv1.Role: 2 admin or 3 member.
    role smallint NOT NULL CHECK (role IN (2, 3)),
    -- SHA-256 of the `sri_` secret. The secret itself is never stored.
    secret_sha256 bytea NOT NULL UNIQUE,
    secret_prefix text NOT NULL,
    created_by_user_id text NOT NULL,
    create_time timestamptz NOT NULL,
    expire_time timestamptz NOT NULL,
    accept_time timestamptz,
    accepted_by_user_id text,
    revoke_time timestamptz
);
CREATE INDEX invitations_org_idx ON invitations (organization_id, invitation_id);

CREATE TABLE api_keys (
    api_key_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    -- Logto `sub` of the key owner.
    user_id text NOT NULL,
    name text NOT NULL,
    -- SHA-256 of the `sandrun_` secret. The secret itself is never stored.
    secret_sha256 bytea NOT NULL UNIQUE,
    display_prefix text NOT NULL,
    create_time timestamptz NOT NULL,
    -- NULL: the key does not expire.
    expire_time timestamptz,
    -- Written at most once per key per minute.
    last_used_time timestamptz,
    revoke_time timestamptz
);
CREATE INDEX api_keys_owner_idx ON api_keys (organization_id, user_id, api_key_id);

-- Audit events have no foreign key: they stay for their 90-day retention
-- after the organization is deleted.
CREATE TABLE audit_events (
    audit_event_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL,
    -- sandrunv1.AuditAction.
    action smallint NOT NULL,
    -- Empty for actions of SandRun itself.
    actor_user_id text NOT NULL,
    actor_api_key_id uuid,
    -- sandrunv1.AuditTargetKind.
    target_kind smallint NOT NULL,
    target_id text NOT NULL,
    subject_user_id text NOT NULL,
    -- Correlation ID of the RPC that wrote the event (the rpc_id log field).
    rpc_id text NOT NULL,
    create_time timestamptz NOT NULL
);
CREATE INDEX audit_events_org_idx ON audit_events (organization_id, audit_event_id DESC);
CREATE INDEX audit_events_time_idx ON audit_events (create_time);

-- The lease that selects the instance that runs the background components.
CREATE TABLE instance_leases (
    lease_name text PRIMARY KEY,
    holder text NOT NULL,
    expire_time timestamptz NOT NULL
);
