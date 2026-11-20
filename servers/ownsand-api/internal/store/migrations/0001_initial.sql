-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Initial OwnSand control plane schema. Contract:
-- docs/servers-ownsand-api-foundation.md#storage.
--
-- Enum columns hold the numeric values of the delino.ownsand.v1 protobuf enums
-- (smallint), so the server never compares free-form state strings. Every
-- organization-owned table has an organization_id column that every query
-- filters on. Secret columns hold only SHA-256 hashes plus a short display
-- prefix. Tables that must outlive the organization row (device removal
-- tombstones, audit events and R2 clean-up jobs) have no foreign key to it.

CREATE TABLE organizations (
    organization_id uuid PRIMARY KEY,
    name text NOT NULL,
    created_at timestamptz NOT NULL,
    -- Set when DeleteOrganization starts; the row is deleted with the
    -- organization's records once the clean-up has run.
    deleted_at timestamptz
);

CREATE TABLE memberships (
    membership_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    user_id text NOT NULL,
    role smallint NOT NULL,
    created_at timestamptz NOT NULL,
    UNIQUE (organization_id, user_id)
);
CREATE INDEX memberships_user_idx ON memberships (user_id);
-- Exactly one owner per organization.
CREATE UNIQUE INDEX memberships_one_owner_idx ON memberships (organization_id) WHERE role = 1;

CREATE TABLE invitations (
    invitation_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    role smallint NOT NULL,
    secret_hash bytea NOT NULL UNIQUE,
    secret_prefix text NOT NULL,
    created_by_kind smallint NOT NULL,
    created_by_id text NOT NULL,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    accepted_at timestamptz,
    accepted_by text,
    revoked_at timestamptz
);
CREATE INDEX invitations_org_idx ON invitations (organization_id, invitation_id);

CREATE TABLE api_keys (
    api_key_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    name text NOT NULL,
    secret_hash bytea NOT NULL UNIQUE,
    secret_prefix text NOT NULL,
    created_by_kind smallint NOT NULL,
    created_by_id text NOT NULL,
    created_at timestamptz NOT NULL,
    revoked_at timestamptz
);
CREATE INDEX api_keys_org_idx ON api_keys (organization_id, api_key_id);

CREATE TABLE devices (
    device_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    name text NOT NULL,
    state smallint NOT NULL,
    -- Raw 32-byte Ed25519 public key.
    public_key bytea NOT NULL,
    -- Encoded delino.ownsand.v1.DeviceCapabilities.
    capabilities bytea NOT NULL,
    limit_vcpu_count bigint NOT NULL DEFAULT 0,
    limit_memory_mib bigint NOT NULL DEFAULT 0,
    limit_disk_gib bigint NOT NULL DEFAULT 0,
    agent_version text NOT NULL DEFAULT '',
    update_state smallint NOT NULL DEFAULT 0,
    -- Time of the first PENDING_IDLE report; survives restarts and lease handovers.
    pending_idle_since timestamptz,
    state_epoch text NOT NULL DEFAULT '',
    enrolled_at timestamptz NOT NULL,
    last_seen_at timestamptz,
    removed_at timestamptz,
    -- ConfirmRemoval succeeded for this device (macOS identifier reservations end).
    removal_confirmed_at timestamptz
);
CREATE INDEX devices_org_idx ON devices (organization_id, device_id);

-- Removal tombstones hold nothing but the device ID, its public key, the
-- removed state (implied by the row) and the expiry. They outlive the device
-- and organization rows.
CREATE TABLE device_tombstones (
    device_id uuid PRIMARY KEY,
    public_key bytea NOT NULL,
    expires_at timestamptz NOT NULL
);
CREATE INDEX device_tombstones_expiry_idx ON device_tombstones (expires_at);

CREATE TABLE enrollment_tokens (
    enrollment_token_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    device_name text NOT NULL,
    secret_hash bytea NOT NULL UNIQUE,
    secret_prefix text NOT NULL,
    created_by_kind smallint NOT NULL,
    created_by_id text NOT NULL,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    used_at timestamptz,
    -- Device and public key of the enrollment, kept until the token expires
    -- so that a retry with the same key returns the same device.
    device_id uuid,
    used_public_key bytea
);
CREATE INDEX enrollment_tokens_org_idx ON enrollment_tokens (organization_id, enrollment_token_id);

CREATE TABLE device_sessions (
    credential_hash bytea PRIMARY KEY,
    device_id uuid NOT NULL REFERENCES devices ON DELETE CASCADE,
    organization_id uuid NOT NULL,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL
);
CREATE INDEX device_sessions_device_idx ON device_sessions (device_id);
CREATE INDEX device_sessions_expiry_idx ON device_sessions (expires_at);

-- Nonces that Authenticate or ConfirmRemoval consumed, kept until expiry to
-- reject replays. CreateChallenge stores nothing.
CREATE TABLE consumed_nonces (
    nonce_hash bytea PRIMARY KEY,
    expires_at timestamptz NOT NULL
);
CREATE INDEX consumed_nonces_expiry_idx ON consumed_nonces (expires_at);

CREATE TABLE snapshots (
    snapshot_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    sandbox_id uuid NOT NULL,
    -- Device that holds the local copy; NULL when no local copy exists.
    device_id uuid,
    state smallint NOT NULL,
    export_state smallint NOT NULL,
    guest_os smallint NOT NULL,
    arch smallint NOT NULL,
    host_os smallint NOT NULL,
    vcpu_count bigint NOT NULL,
    memory_mib bigint NOT NULL,
    disk_gib bigint NOT NULL,
    network_mode smallint NOT NULL,
    has_memory_state boolean NOT NULL DEFAULT false,
    size_bytes bigint NOT NULL DEFAULT 0,
    macos_hardware_model text NOT NULL DEFAULT '',
    macos_machine_identifier bytea,
    -- SHA-256 of each exported part, in part order.
    part_sha256 bytea[] NOT NULL DEFAULT '{}',
    export_part_count integer NOT NULL DEFAULT 0,
    created_by_kind smallint NOT NULL,
    created_by_id text NOT NULL,
    created_at timestamptz NOT NULL
);
CREATE INDEX snapshots_org_idx ON snapshots (organization_id, snapshot_id);
CREATE INDEX snapshots_device_idx ON snapshots (device_id);

CREATE TABLE templates (
    template_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    name text NOT NULL,
    snapshot_id uuid NOT NULL REFERENCES snapshots ON DELETE CASCADE,
    created_by_kind smallint NOT NULL,
    created_by_id text NOT NULL,
    created_at timestamptz NOT NULL,
    UNIQUE (organization_id, name)
);
CREATE INDEX templates_snapshot_idx ON templates (snapshot_id);

CREATE TABLE sandboxes (
    sandbox_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    device_id uuid NOT NULL,
    -- Caller-generated create key, kept with the reservation for 24 hours.
    idempotency_key uuid,
    -- SHA-256 of the deterministic encoding of the create parameters.
    idempotency_hash bytea,
    idempotency_expires_at timestamptz,
    source_kind smallint NOT NULL,
    base_image smallint NOT NULL DEFAULT 0,
    source_snapshot_id uuid,
    source_template_id uuid,
    guest_os smallint NOT NULL,
    arch smallint NOT NULL,
    vcpu_count bigint NOT NULL,
    memory_mib bigint NOT NULL,
    disk_gib bigint NOT NULL,
    network_mode smallint NOT NULL,
    idle_ttl_ms bigint NOT NULL,
    max_ttl_ms bigint NOT NULL,
    idle_deadline timestamptz NOT NULL,
    max_deadline timestamptz NOT NULL,
    labels jsonb NOT NULL DEFAULT '{}',
    created_by_kind smallint NOT NULL,
    created_by_id text NOT NULL,
    created_at timestamptz NOT NULL,
    -- State that the agent reported (or that the server set at create or
    -- reconciliation). Reachability is derived from the device state.
    agent_state smallint NOT NULL,
    end_reason smallint NOT NULL DEFAULT 0,
    ended_at timestamptz,
    revision_epoch text NOT NULL DEFAULT '',
    revision_counter bigint NOT NULL DEFAULT 0,
    restore_path smallint NOT NULL DEFAULT 0,
    -- The agent reported the sandbox at least once (its disk counts in the
    -- device's reported free disk from then on).
    agent_reported boolean NOT NULL DEFAULT false,
    -- Pending delete, stored apart from the agent state.
    pending_delete boolean NOT NULL DEFAULT false,
    delete_reason smallint NOT NULL DEFAULT 0,
    delete_actor_kind smallint NOT NULL DEFAULT 0,
    delete_actor_id text NOT NULL DEFAULT '',
    macos_machine_identifier bytea,
    UNIQUE (organization_id, idempotency_key)
);
CREATE INDEX sandboxes_org_idx ON sandboxes (organization_id, sandbox_id);
CREATE INDEX sandboxes_device_idx ON sandboxes (device_id, agent_state);
CREATE INDEX sandboxes_machine_identifier_idx ON sandboxes (macos_machine_identifier) WHERE macos_machine_identifier IS NOT NULL;

CREATE TABLE agent_version_policies (
    organization_id uuid PRIMARY KEY REFERENCES organizations ON DELETE CASCADE,
    mode smallint NOT NULL,
    pinned_version text NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL
);

-- Releases from the signed release manifest.
CREATE TABLE agent_releases (
    version text NOT NULL,
    -- "<host_os>/<arch>" in lowercase, for example "linux/amd64".
    platform text NOT NULL,
    is_default boolean NOT NULL DEFAULT false,
    revoked boolean NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (version, platform)
);

CREATE TABLE audit_events (
    audit_event_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL,
    action smallint NOT NULL,
    actor_kind smallint NOT NULL,
    actor_id text NOT NULL,
    end_reason smallint NOT NULL DEFAULT 0,
    target_kind smallint NOT NULL,
    target_id text NOT NULL,
    request_id text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL
);
CREATE INDEX audit_events_created_idx ON audit_events (created_at);
CREATE INDEX audit_events_org_idx ON audit_events (organization_id, created_at);

-- Single active instance lease. Contract: Runtime and Language.
CREATE TABLE service_leases (
    name text PRIMARY KEY,
    holder text NOT NULL,
    expires_at timestamptz NOT NULL
);

-- Cross-instance events: revocations (API key, membership, sandbox delete),
-- configuration changes (device limits, agent version policy), pending
-- deletes and device removals. Any instance writes them in the transaction
-- of the change; the lease holder consumes them in id order.
CREATE TABLE control_events (
    event_id bigserial PRIMARY KEY,
    kind smallint NOT NULL,
    organization_id uuid NOT NULL,
    api_key_id uuid,
    user_id text,
    sandbox_id uuid,
    device_id uuid,
    created_at timestamptz NOT NULL
);
CREATE INDEX control_events_created_idx ON control_events (created_at);

-- R2 clean-up state of a deleted organization or a deleted exported snapshot.
CREATE TABLE r2_cleanup_jobs (
    cleanup_job_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL,
    -- Object key prefix to delete, ending with "/".
    prefix text NOT NULL,
    -- The last signed upload URL under the prefix expires at this time; the
    -- final sweep runs after it.
    final_sweep_after timestamptz NOT NULL,
    attempts integer NOT NULL DEFAULT 0,
    last_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    UNIQUE (prefix)
);

-- Last signed upload URL issued per organization and snapshot, so that the
-- clean-up job knows when every upload URL has expired.
CREATE TABLE signed_upload_issues (
    organization_id uuid NOT NULL,
    snapshot_id uuid NOT NULL,
    last_expires_at timestamptz NOT NULL,
    PRIMARY KEY (organization_id, snapshot_id)
);
