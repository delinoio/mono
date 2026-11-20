-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Initial ownsand-api schema. Enum columns hold the numeric values of the
-- delino.ownsand.v1 protobuf enums. Every organization-owned table has an
-- organization_id column that every query filters on. Secret columns hold
-- only SHA-256 hashes.

CREATE TABLE organizations (
    organization_id uuid PRIMARY KEY,
    name text NOT NULL,
    create_time timestamptz NOT NULL
);

CREATE TABLE memberships (
    membership_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    user_id text NOT NULL,
    role smallint NOT NULL,
    join_time timestamptz NOT NULL,
    UNIQUE (organization_id, user_id)
);
CREATE INDEX memberships_user_idx ON memberships (user_id, organization_id);
-- Exactly one owner per organization (role 1 = ROLE_OWNER).
CREATE UNIQUE INDEX memberships_one_owner_idx ON memberships (organization_id) WHERE role = 1;

CREATE TABLE invitations (
    invitation_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    role smallint NOT NULL,
    secret_sha256 bytea NOT NULL UNIQUE,
    secret_prefix text NOT NULL,
    created_by_kind smallint NOT NULL,
    created_by_id text NOT NULL,
    create_time timestamptz NOT NULL,
    expire_time timestamptz NOT NULL,
    accept_time timestamptz,
    accepted_by text,
    revoke_time timestamptz
);
CREATE INDEX invitations_org_idx ON invitations (organization_id, invitation_id);

CREATE TABLE api_keys (
    api_key_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    name text NOT NULL,
    secret_sha256 bytea NOT NULL UNIQUE,
    key_prefix text NOT NULL,
    created_by_kind smallint NOT NULL,
    created_by_id text NOT NULL,
    create_time timestamptz NOT NULL,
    revoke_time timestamptz
);
CREATE INDEX api_keys_org_idx ON api_keys (organization_id, api_key_id);

CREATE TABLE enrollment_tokens (
    enrollment_token_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    secret_sha256 bytea NOT NULL UNIQUE,
    token_prefix text NOT NULL,
    created_by_kind smallint NOT NULL,
    created_by_id text NOT NULL,
    create_time timestamptz NOT NULL,
    expire_time timestamptz NOT NULL,
    use_time timestamptz,
    device_id uuid,
    device_public_key bytea,
    revoke_time timestamptz
);
CREATE INDEX enrollment_tokens_org_idx ON enrollment_tokens (organization_id, enrollment_token_id);

-- Devices. A removed device keeps a tombstone row (device_id, public key,
-- state REMOVED and tombstone_expire_time) also after its organization is
-- deleted, so organization_id has no foreign key.
CREATE TABLE devices (
    device_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL,
    public_key bytea NOT NULL,
    name text NOT NULL,
    host_os smallint NOT NULL,
    arch smallint NOT NULL,
    agent_version text NOT NULL,
    update_state smallint NOT NULL DEFAULT 0,
    update_target_version text NOT NULL DEFAULT '',
    pending_idle_since timestamptz,
    capabilities bytea,
    limit_vcpu_count integer NOT NULL DEFAULT 0,
    limit_memory_mib bigint NOT NULL DEFAULT 0,
    limit_disk_gib bigint NOT NULL DEFAULT 0,
    vcpu_overcommit_percent integer NOT NULL DEFAULT 100,
    state smallint NOT NULL,
    state_epoch text NOT NULL DEFAULT '',
    create_time timestamptz NOT NULL,
    last_seen_time timestamptz,
    remove_time timestamptz,
    tombstone_expire_time timestamptz
);
CREATE INDEX devices_org_idx ON devices (organization_id, device_id);
CREATE INDEX devices_tombstone_idx ON devices (tombstone_expire_time) WHERE tombstone_expire_time IS NOT NULL;

CREATE TABLE device_sessions (
    credential_sha256 bytea PRIMARY KEY,
    device_id uuid NOT NULL REFERENCES devices ON DELETE CASCADE,
    expire_time timestamptz NOT NULL
);
CREATE INDEX device_sessions_device_idx ON device_sessions (device_id);

-- Nonces that Authenticate or ConfirmRemoval consumed, until their expiry.
CREATE TABLE consumed_nonces (
    nonce_sha256 bytea PRIMARY KEY,
    expire_time timestamptz NOT NULL
);

CREATE TABLE snapshots (
    snapshot_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    source_sandbox_id uuid NOT NULL,
    -- Device that holds the original local copy; NULL when no device holds
    -- one (an exported snapshot whose device was removed).
    device_id uuid,
    state smallint NOT NULL,
    export_state smallint NOT NULL,
    guest_os smallint NOT NULL,
    host_os smallint NOT NULL,
    arch smallint NOT NULL,
    vcpu_count integer NOT NULL,
    memory_mib bigint NOT NULL,
    disk_gib bigint NOT NULL,
    network_mode smallint NOT NULL,
    has_memory_state boolean NOT NULL DEFAULT false,
    size_bytes bigint NOT NULL DEFAULT 0,
    macos_hardware_model bytea,
    -- Sandbox whose VM the machine identifier of a macOS snapshot belongs
    -- to (the root of the snapshot lineage).
    machine_lineage uuid,
    export_part_size_bytes bigint NOT NULL DEFAULT 0,
    created_by_kind smallint NOT NULL,
    created_by_id text NOT NULL,
    create_time timestamptz NOT NULL
);
CREATE INDEX snapshots_org_idx ON snapshots (organization_id, snapshot_id);
CREATE INDEX snapshots_device_idx ON snapshots (device_id);

CREATE TABLE snapshot_parts (
    snapshot_id uuid NOT NULL REFERENCES snapshots ON DELETE CASCADE,
    part_number integer NOT NULL,
    size_bytes bigint NOT NULL,
    sha256 bytea NOT NULL,
    PRIMARY KEY (snapshot_id, part_number)
);

-- Devices that imported an exported snapshot and hold a copy of it.
CREATE TABLE snapshot_copies (
    snapshot_id uuid NOT NULL REFERENCES snapshots ON DELETE CASCADE,
    device_id uuid NOT NULL,
    PRIMARY KEY (snapshot_id, device_id)
);

CREATE TABLE templates (
    template_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    name text NOT NULL,
    snapshot_id uuid NOT NULL REFERENCES snapshots ON DELETE CASCADE,
    created_by_kind smallint NOT NULL,
    created_by_id text NOT NULL,
    create_time timestamptz NOT NULL,
    UNIQUE (organization_id, name)
);

CREATE TABLE sandboxes (
    sandbox_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    device_id uuid NOT NULL,
    -- State that the agent reported last, or that the server set itself
    -- (CREATING at create, reconciliation results). Reachability is derived
    -- from the device state and is not stored here.
    agent_state smallint NOT NULL,
    end_reason smallint,
    revision_epoch text NOT NULL DEFAULT '',
    revision_counter bigint NOT NULL DEFAULT 0,
    -- Pending delete, stored apart from the agent state.
    delete_pending boolean NOT NULL DEFAULT false,
    delete_end_reason smallint,
    delete_actor_kind smallint,
    delete_actor_id text,
    source_base_image smallint,
    source_snapshot_id uuid,
    source_template_id uuid,
    -- Snapshot that a template source resolved to.
    resolved_snapshot_id uuid,
    vcpu_count integer NOT NULL,
    memory_mib bigint NOT NULL,
    disk_gib bigint NOT NULL,
    guest_os smallint NOT NULL,
    host_os smallint NOT NULL,
    arch smallint NOT NULL,
    network_mode smallint NOT NULL,
    labels jsonb NOT NULL DEFAULT '{}'::jsonb,
    idle_ttl_ms bigint NOT NULL,
    max_ttl_ms bigint NOT NULL,
    idle_deadline timestamptz,
    max_deadline timestamptz NOT NULL,
    restore_path smallint NOT NULL DEFAULT 0,
    machine_lineage uuid,
    created_by_kind smallint NOT NULL,
    created_by_id text NOT NULL,
    create_time timestamptz NOT NULL,
    end_time timestamptz
);
CREATE INDEX sandboxes_org_idx ON sandboxes (organization_id, sandbox_id);
CREATE INDEX sandboxes_device_idx ON sandboxes (device_id, agent_state);
CREATE INDEX sandboxes_lineage_idx ON sandboxes (machine_lineage) WHERE machine_lineage IS NOT NULL;

-- Create idempotency reservations, kept for 24 hours.
CREATE TABLE sandbox_create_keys (
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    idempotency_key uuid NOT NULL,
    request_sha256 bytea NOT NULL,
    sandbox_id uuid NOT NULL,
    expire_time timestamptz NOT NULL,
    PRIMARY KEY (organization_id, idempotency_key)
);

CREATE TABLE agent_version_policies (
    organization_id uuid PRIMARY KEY REFERENCES organizations ON DELETE CASCADE,
    mode smallint NOT NULL,
    pinned_version text NOT NULL DEFAULT '',
    update_time timestamptz NOT NULL,
    updated_by_kind smallint NOT NULL,
    updated_by_id text NOT NULL
);

-- Releases from the signed release manifest, per platform.
CREATE TABLE agent_releases (
    version text NOT NULL,
    host_os smallint NOT NULL,
    arch smallint NOT NULL,
    is_default boolean NOT NULL DEFAULT false,
    revoked boolean NOT NULL DEFAULT false,
    PRIMARY KEY (version, host_os, arch)
);
CREATE UNIQUE INDEX agent_releases_one_default_idx ON agent_releases (host_os, arch) WHERE is_default;

-- Audit events are kept for 90 days, also after organization deletion, so
-- organization_id has no foreign key.
CREATE TABLE audit_events (
    audit_event_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL,
    action smallint NOT NULL,
    actor_kind smallint NOT NULL,
    actor_id text NOT NULL,
    end_reason smallint,
    target_kind smallint NOT NULL,
    target_id text NOT NULL,
    request_id text NOT NULL,
    create_time timestamptz NOT NULL
);
CREATE INDEX audit_events_org_idx ON audit_events (organization_id, create_time);
CREATE INDEX audit_events_time_idx ON audit_events (create_time);

-- Events that any instance writes in the transaction of a revoking or
-- configuration-changing request, and that the lease holder processes.
CREATE TABLE control_events (
    event_id bigserial PRIMARY KEY,
    kind smallint NOT NULL,
    organization_id uuid,
    device_id uuid,
    sandbox_id uuid,
    api_key_id uuid,
    user_id text,
    create_time timestamptz NOT NULL DEFAULT now()
);

-- R2 clean-up jobs: organization deletion and snapshot export deletion.
CREATE TABLE r2_cleanup_jobs (
    job_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL,
    prefix text NOT NULL,
    create_time timestamptz NOT NULL,
    -- The job deletes the prefix once more after every signed upload URL
    -- that was issued for it expired.
    final_sweep_after timestamptz NOT NULL,
    first_sweep_done boolean NOT NULL DEFAULT false,
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_time timestamptz NOT NULL
);
