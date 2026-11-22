-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Configuration entities of an organization: the OwnSand link, agent
-- integrations with their encrypted credentials, and runner profiles. Enum
-- columns store the protobuf enum numbers of delino.tide.v1. Encrypted
-- values are AES-256-GCM output (package secrets) with the ID of their key
-- in the column next to them. The owning contract is
-- docs/servers-tide-api-foundation.md (OwnSand link, Agent integrations,
-- Runner profiles, Secrets).

-- +goose Up
CREATE TABLE ownsand_links (
    organization_id uuid PRIMARY KEY REFERENCES organizations ON DELETE CASCADE,
    -- New for each link; a key replacement keeps it. Audit events name it.
    ownsand_link_id uuid NOT NULL UNIQUE,
    -- One OwnSand organization links to at most one tide organization.
    ownsand_organization_id uuid NOT NULL UNIQUE,
    -- tidev1.OwnSandLinkState: 1 active, 2 invalid.
    state smallint NOT NULL CHECK (state IN (1, 2)),
    -- Set while RemoveOwnSandLink deletes the runners: dispatch stops.
    removing boolean NOT NULL DEFAULT false,
    api_key_key_id text NOT NULL,
    api_key_ciphertext bytea NOT NULL,
    credential_set_time timestamptz NOT NULL,
    create_time timestamptz NOT NULL
);

CREATE TABLE agent_integrations (
    agent_integration_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    -- tidev1.AgentVendor.
    vendor smallint NOT NULL CHECK (vendor IN (1, 2, 3, 4)),
    name text NOT NULL,
    -- CURSOR settings.
    pool_name text NOT NULL DEFAULT '',
    -- DEVIN settings.
    outpost_name text NOT NULL DEFAULT '',
    create_time timestamptz NOT NULL,
    update_time timestamptz NOT NULL,
    UNIQUE (organization_id, name),
    -- Lets runner_profiles reference an integration of its own organization.
    UNIQUE (organization_id, agent_integration_id)
);

CREATE TABLE agent_integration_credentials (
    agent_integration_id uuid NOT NULL REFERENCES agent_integrations ON DELETE CASCADE,
    -- tidev1.AgentCredential.
    credential smallint NOT NULL CHECK (credential BETWEEN 1 AND 5),
    key_id text NOT NULL,
    ciphertext bytea NOT NULL,
    set_time timestamptz NOT NULL,
    PRIMARY KEY (agent_integration_id, credential)
);

CREATE TABLE runner_profiles (
    runner_profile_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    name text NOT NULL,
    -- tidev1.WorkloadKind.
    workload_kind smallint NOT NULL CHECK (workload_kind IN (1, 2, 3)),
    agent_integration_id uuid,
    workspace_path text NOT NULL DEFAULT '',
    -- tidev1.GuestOs.
    guest_os smallint NOT NULL CHECK (guest_os IN (1, 2, 3)),
    -- Exactly one source: an OwnSand base image (tidev1.GuestOs) or an
    -- OwnSand template ID.
    base_image smallint,
    ownsand_template_id uuid,
    vcpu_count integer NOT NULL,
    memory_mib bigint NOT NULL,
    disk_gib bigint NOT NULL,
    -- tidev1.NetworkMode.
    network_mode smallint NOT NULL,
    idle_ttl_ms bigint NOT NULL,
    max_ttl_ms bigint NOT NULL,
    -- tidev1.Priority.
    priority smallint NOT NULL CHECK (priority IN (1, 2, 3)),
    max_queue_wait_ms bigint NOT NULL,
    warm_count integer NOT NULL CHECK (warm_count BETWEEN 0 AND 10),
    create_time timestamptz NOT NULL,
    update_time timestamptz NOT NULL,
    UNIQUE (organization_id, name),
    CHECK ((base_image IS NULL) <> (ownsand_template_id IS NULL)),
    -- An integration that a profile names cannot be deleted.
    FOREIGN KEY (organization_id, agent_integration_id) REFERENCES agent_integrations (organization_id, agent_integration_id)
);
CREATE INDEX runner_profiles_integration_idx ON runner_profiles (agent_integration_id) WHERE agent_integration_id IS NOT NULL;
