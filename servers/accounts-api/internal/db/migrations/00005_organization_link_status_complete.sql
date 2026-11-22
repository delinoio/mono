-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- A complete billing status of an organization link. Owning contract:
-- docs/servers-accounts-api-foundation.md (Usage reports and billing status,
-- Storage). The change only rejects rows that no release writes, so the
-- previous release keeps working while the new one starts.

-- +goose Up

-- The constraint of migration 00003 compared nullable columns with = and <>.
-- A NULL operand makes the whole branch NULL, and a CHECK constraint accepts
-- NULL, so a row with a NULL billing_status or status_reason and a non-null
-- status_sequence, blocked_since or suspend_time passed. State IS NOT NULL
-- for both columns in the branches that compare them, so only the three
-- complete shapes remain: no status, an ALLOWED status without timestamps,
-- and a BLOCKED or SUSPENDED status with a reason other than none, a
-- sequence and both timestamps. The billing status worker writes all status
-- columns in one statement, so existing rows satisfy the stricter check.
ALTER TABLE organization_links
    DROP CONSTRAINT organization_links_status_complete,
    ADD CONSTRAINT organization_links_status_complete CHECK (
        (billing_status IS NULL AND status_reason IS NULL AND status_sequence IS NULL
            AND blocked_since IS NULL AND suspend_time IS NULL)
        OR (billing_status IS NOT NULL AND billing_status = 'allowed'
            AND status_reason IS NOT NULL AND status_reason = 'none'
            AND status_sequence IS NOT NULL
            AND blocked_since IS NULL AND suspend_time IS NULL)
        OR (billing_status IS NOT NULL AND billing_status IN ('blocked', 'suspended')
            AND status_reason IS NOT NULL AND status_reason <> 'none'
            AND status_sequence IS NOT NULL
            AND blocked_since IS NOT NULL AND suspend_time IS NOT NULL));
