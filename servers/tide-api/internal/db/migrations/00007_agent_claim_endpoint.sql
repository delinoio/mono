-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Vendors that push their demand to tide (Claude Code through the
-- spawn-runner hook, OpenAI Codex through webhooks) store the request's
-- vendor identity in agent_claims too, without a claim at the vendor. The
-- endpoint is a non-secret address that the runner program needs (OpenAI
-- Codex: the session's environment.remote_url), and workspace_directory the
-- guest directory that the session expects (OpenAI Codex:
-- environment.workspace_directory); both are empty for the other vendors. The row still holds no credential, token or work order. The
-- owning contract is docs/servers-tide-api-foundation.md (Agent
-- integrations, Storage).

-- +goose Up
ALTER TABLE agent_claims ADD COLUMN endpoint text NOT NULL DEFAULT '',
    ADD COLUMN workspace_directory text NOT NULL DEFAULT '';
