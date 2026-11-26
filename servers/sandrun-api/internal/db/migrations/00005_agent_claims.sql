-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Vendor claims of AGENT_SESSION requests (Cursor and Devin). Each row is
-- the claim that SandRun holds, or held, for one request: the vendor request
-- ID (Cursor request ID, Devin session ID), the worker ID or acceptor ID
-- that SandRun chose for the claim and that the runner program uses, and the
-- scope that the claim names (Cursor pool name, Devin outpost ID). The row
-- holds no credential, token or work order. The owning contract is
-- docs/servers-sandrun-api-foundation.md (Cursor and Devin, Storage).

-- +goose Up
CREATE TABLE agent_claims (
    request_id uuid PRIMARY KEY REFERENCES requests ON DELETE CASCADE,
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    agent_integration_id uuid NOT NULL REFERENCES agent_integrations ON DELETE CASCADE,
    -- sandrunv1.AgentVendor.
    vendor smallint NOT NULL,
    vendor_request_id text NOT NULL,
    -- The demand marker of the vendor entry (Cursor createdAtMs, Devin
    -- metadata.created_at), so that SandRun does not claim an entry again
    -- that a user canceled in SandRun (the request's end reason records the
    -- cancellation).
    demand_marker text NOT NULL DEFAULT '',
    worker_id text NOT NULL,
    scope text NOT NULL DEFAULT '',
    -- agent.ClaimState: 1 held, 2 released (or no longer SandRun's).
    state smallint NOT NULL CHECK (state IN (1, 2)),
    create_time timestamptz NOT NULL,
    update_time timestamptz NOT NULL
);
CREATE INDEX agent_claims_vendor_idx ON agent_claims (organization_id, vendor, vendor_request_id, create_time);
CREATE INDEX agent_claims_held_idx ON agent_claims (agent_integration_id) WHERE state = 1;
