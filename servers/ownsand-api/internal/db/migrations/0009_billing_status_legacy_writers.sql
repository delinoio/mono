-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Legacy-writer compatibility of the billing status rows. Migration 0008
-- backfilled one organization_billing row for each organization that
-- existed when it ran, but the previous release keeps serving while a new
-- container applies the migrations (a rolling deployment) and its
-- CreateOrganization inserts an organization without a status row. The
-- billing-aware code treats an organization without a status row as
-- missing, so such an organization could never link a billing account. The
-- owning contract is the Billing section of
-- docs/servers-ownsand-api-foundation.md.
--
-- This migration makes the database create the missing row itself:
--   1. A transaction-scoped lock on organizations that conflicts with
--      concurrent inserts, so the backfill below sees every organization
--      that committed before it and no insert slips between the backfill
--      and the trigger: an insert that started earlier is waited for, and an
--      insert that starts later waits for this transaction and then fires
--      the trigger.
--   2. A deferred constraint trigger on organizations that inserts the
--      initial status row when the inserting transaction commits. The
--      billing-aware writer inserts its own row in the transaction, so the
--      trigger finds it and does nothing (ON CONFLICT DO NOTHING). Deferring
--      the trigger to the commit also covers a legacy writer that deletes
--      the organization again in the same transaction: no row is created
--      for an organization that no longer exists.
--   3. A backfill of the organizations that still have no row.
--
-- Numeric values (hard-coded because migrations are immutable and
-- independent of the generated code): phase 2 is BILLING_PHASE_BLOCKED and
-- reason 1 is BILLING_REASON_ACCOUNT_REQUIRED in
-- protos/delino/ownsand/v1/billing.proto and common.proto; suspend_time is
-- blocked_since plus 3 days (72 hours, the SuspendAfter of the billing
-- package). A renumbered enum value needs a new migration.
--
-- Removal condition (recorded): the trigger stays until a later contract
-- migration drops it, which is allowed once no release that predates
-- migration 0008 can run against the database. That migration drops
-- organizations_billing_status_insert and create_organization_billing_status.

-- +goose Up

LOCK TABLE organizations IN SHARE ROW EXCLUSIVE MODE;

-- +goose StatementBegin
CREATE FUNCTION create_organization_billing_status() RETURNS trigger AS $$
BEGIN
    -- The organization can be gone at commit (inserted and deleted in one
    -- transaction); the foreign key would reject the row then.
    INSERT INTO organization_billing (organization_id, billing_account_id, phase, reason, blocked_since, suspend_time, update_time)
    SELECT NEW.organization_id, NULL, 2, 1, NEW.create_time, NEW.create_time + interval '72 hours', now()
    WHERE EXISTS (SELECT 1 FROM organizations WHERE organization_id = NEW.organization_id)
    ON CONFLICT (organization_id) DO NOTHING;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER organizations_billing_status_insert
    AFTER INSERT ON organizations
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    EXECUTE FUNCTION create_organization_billing_status();

-- Backfill: the organizations that a legacy writer created after
-- migration 0008. The block starts at the later of the creation and
-- migration 0008, because billing was not enforced before it; existing rows
-- keep their links and timestamps.
--
-- goose records the application time of migration 0008 in the tstamp column
-- (timestamp without time zone) of goose_db_version. The migration runner in
-- internal/db opens its sessions with TimeZone=UTC, so the column holds UTC.
INSERT INTO organization_billing (organization_id, billing_account_id, phase, reason, blocked_since, suspend_time, update_time)
SELECT o.organization_id, NULL, 2, 1, s.blocked_since, s.blocked_since + interval '72 hours', now()
FROM organizations o
CROSS JOIN LATERAL (
    SELECT greatest(o.create_time, (
        SELECT tstamp AT TIME ZONE 'UTC' FROM goose_db_version
        WHERE version_id = 8 AND is_applied ORDER BY id DESC LIMIT 1
    )) AS blocked_since
) s
WHERE NOT EXISTS (SELECT 1 FROM organization_billing b WHERE b.organization_id = o.organization_id);
