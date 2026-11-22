-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Allows the OPENAI_CODEX credential AGENT_CREDENTIAL_API_KEY (6): an
-- OpenAI API key with api.agents.read, which tide uses to read the session
-- of an agent.session.action_required webhook. The owning contract is
-- docs/servers-tide-api-foundation.md (OpenAI Codex, Secrets).

-- +goose Up
ALTER TABLE agent_integration_credentials DROP CONSTRAINT agent_integration_credentials_credential_check;
ALTER TABLE agent_integration_credentials ADD CONSTRAINT agent_integration_credentials_credential_check CHECK (credential BETWEEN 1 AND 6);
