-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Webhook deliveries (shared by every webhook sender) and the GitHub App
-- layer: installation links, single-use installation states, the GitHub job
-- of each GITHUB_JOB request and the GitHub runner registration of each
-- GITHUB_JOB runner. Enum columns store the protobuf enum numbers of
-- delino.sandrun.v1; webhook_deliveries.sender stores webhook.Sender. The
-- owning contract is docs/servers-sandrun-api-foundation.md (GitHub App
-- integration, Webhooks, Storage).

-- +goose Up
CREATE TABLE webhook_deliveries (
    -- webhook.Sender: 1 GitHub, 2 OpenAI.
    sender smallint NOT NULL,
    -- The sender's delivery ID (GitHub: X-GitHub-Delivery, which a
    -- redelivery repeats).
    delivery_id text NOT NULL,
    receive_time timestamptz NOT NULL,
    PRIMARY KEY (sender, delivery_id)
);
CREATE INDEX webhook_deliveries_time_idx ON webhook_deliveries (receive_time);

CREATE TABLE github_installations (
    installation_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    -- One GitHub installation links to exactly one SandRun organization.
    github_installation_id bigint NOT NULL UNIQUE,
    github_account_login text NOT NULL,
    github_account_id bigint NOT NULL,
    -- sandrunv1.GitHubInstallationState.
    state smallint NOT NULL,
    github_runner_group_id bigint NOT NULL,
    runner_group_name text NOT NULL,
    linked_by_user_id text NOT NULL,
    create_time timestamptz NOT NULL,
    update_time timestamptz NOT NULL
);
CREATE INDEX github_installations_org_idx ON github_installations (organization_id, installation_id);

-- The single-use `state` of CreateGitHubInstallUrl; only its SHA-256 hash.
CREATE TABLE github_install_states (
    state_hash bytea PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    user_id text NOT NULL,
    expire_time timestamptz NOT NULL
);
CREATE INDEX github_install_states_expire_idx ON github_install_states (expire_time);

-- The GitHub job of a GITHUB_JOB request, for the API.
CREATE TABLE github_jobs (
    request_id uuid PRIMARY KEY REFERENCES requests ON DELETE CASCADE,
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    github_installation_id bigint NOT NULL,
    github_job_id bigint NOT NULL,
    github_workflow_run_id bigint NOT NULL,
    repository_full_name text NOT NULL,
    job_name text NOT NULL
);

-- The GitHub runner registration of a GITHUB_JOB runner. The JIT
-- configuration itself is never stored.
CREATE TABLE github_runners (
    runner_id uuid PRIMARY KEY REFERENCES runners ON DELETE CASCADE,
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    github_installation_id bigint NOT NULL,
    -- The label sandrun-<profile name> of the registration. A runner whose
    -- profile was renamed since keeps the old label.
    github_label text NOT NULL,
    -- Set when GitHub created the JIT configuration.
    github_runner_id bigint,
    -- true after SandRun removed the registration (or found it gone).
    registration_removed boolean NOT NULL DEFAULT false
);
