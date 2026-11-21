-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Billing status cache, billing links and billing enforcement: one status
-- row per organization, the cursor of the status poll, the pending unlinks
-- that delino-accounts has not confirmed yet and the billing pause mark of
-- sandboxes. The owning contract is the Billing section of
-- docs/servers-ownsand-api-foundation.md.
--
-- phase holds the delino.ownsand.v1 BillingPhase value and reason the
-- BillingReason value of the status that delino-accounts reported for a
-- linked organization, or that the control plane decided for an
-- organization without a link.

CREATE TABLE organization_billing (
    organization_id uuid PRIMARY KEY REFERENCES organizations ON DELETE CASCADE,
    -- Linked delino-accounts billing account; NULL without a link.
    billing_account_id uuid,
    phase smallint NOT NULL,
    reason smallint NOT NULL,
    -- Start of the non-payment; NULL while the phase is ALLOWED.
    blocked_since timestamptz,
    -- blocked_since plus 3 days; NULL while the phase is ALLOWED.
    suspend_time timestamptz,
    -- Last time delino-accounts confirmed the status of a linked
    -- organization; NULL without a link.
    status_sync_time timestamptz,
    -- Sequence of the last applied delino-accounts status.
    status_sequence bigint NOT NULL DEFAULT 0,
    update_time timestamptz NOT NULL,
    CHECK ((phase = 1) = (blocked_since IS NULL)),
    CHECK (billing_account_id IS NOT NULL OR phase <> 1)
);
CREATE INDEX organization_billing_linked_idx ON organization_billing (billing_account_id) WHERE billing_account_id IS NOT NULL;

-- Rollout (recorded decision): billing is enforced for every organization
-- when it is deployed. An organization that exists now has no link, so it
-- is BLOCKED with ACCOUNT_REQUIRED from this migration (the billing launch)
-- and SUSPENDED 3 days later.
INSERT INTO organization_billing (organization_id, billing_account_id, phase, reason, blocked_since, suspend_time, update_time)
SELECT organization_id, NULL, 2, 1, now(), now() + interval '3 days', now() FROM organizations;

-- One row: the cursor of ListBillingStatusChanges.
CREATE TABLE billing_status_cursor (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    after_sequence bigint NOT NULL CHECK (after_sequence >= 0),
    poll_time timestamptz
);
INSERT INTO billing_status_cursor (singleton, after_sequence) VALUES (true, 0);

-- Links that TransferOwnership or DeleteOrganization removed locally and
-- that delino-accounts has not confirmed yet. No foreign key: a deleted
-- organization keeps its row until UnlinkOrganization succeeds.
CREATE TABLE billing_pending_unlinks (
    organization_id uuid PRIMARY KEY,
    billing_account_id uuid NOT NULL,
    create_time timestamptz NOT NULL,
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_time timestamptz NOT NULL
);

-- Set on a sandbox that the suspension worker paused (or is pausing), so
-- that a recovery resumes only those sandboxes.
ALTER TABLE sandboxes ADD COLUMN paused_by_billing boolean NOT NULL DEFAULT false;
CREATE INDEX sandboxes_paused_by_billing_idx ON sandboxes (organization_id) WHERE paused_by_billing;
CREATE INDEX sandboxes_running_idx ON sandboxes (organization_id) WHERE agent_state = 2;
