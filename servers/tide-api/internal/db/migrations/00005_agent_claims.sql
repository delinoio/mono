-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Vendor claims of AGENT_SESSION requests (Cursor and Devin). Each row is
-- the claim that tide holds, or held, for one request: the vendor request
-- ID (Cursor request ID, Devin session ID), the worker ID or acceptor ID
-- that tide chose for the claim and that the runner program uses, and the
-- scope that the claim names (Cursor pool name, Devin outpost ID). The row
-- holds no credential, token or work order. The owning contract is
-- docs/servers-tide-api-foundation.md (Cursor and Devin, Storage).

-- +goose Up
CREATE TABLE agent_claims (
    request_id uuid PRIMARY KEY REFERENCES requests ON DELETE CASCADE,
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    agent_integration_id uuid NOT NULL REFERENCES agent_integrations ON DELETE CASCADE,
    -- tidev1.AgentVendor.
    vendor smallint NOT NULL,
    vendor_request_id text NOT NULL,
    -- The demand marker of the vendor entry (Cursor createdAtMs, Devin
    -- metadata.created_at), so that tide does not claim an entry again
    -- that a user canceled in tide.
    demand_marker text NOT NULL DEFAULT '',
    worker_id text NOT NULL,
    scope text NOT NULL DEFAULT '',
    -- agent.ClaimState: 1 held, 2 released (or no longer tide's).
    state smallint NOT NULL CHECK (state IN (1, 2)),
    -- A user canceled the request in tide before a runner took it.
    declined boolean NOT NULL DEFAULT false,
    create_time timestamptz NOT NULL,
    update_time timestamptz NOT NULL
);
CREATE INDEX agent_claims_vendor_idx ON agent_claims (organization_id, vendor, vendor_request_id, create_time);
CREATE INDEX agent_claims_held_idx ON agent_claims (agent_integration_id) WHERE state = 1;
