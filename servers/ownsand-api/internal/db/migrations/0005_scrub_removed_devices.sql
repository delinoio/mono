-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- A removed device keeps only a tombstone: device_id, organization_id (for
-- scoped reads), public_key, state, create_time, remove_time and
-- tombstone_expire_time. Every other column is device inventory, and this
-- trigger scrubs it at the write boundary, so no writer (the removal
-- transaction, a late capability report, a state report that merges built
-- images, a heartbeat that records update fields) can store or restore it on
-- a row whose state is REMOVED.
--
-- Columns that are NOT NULL get the neutral value of their initial default
-- (the empty string, 0, or 100 for vcpu_overcommit_percent); nullable
-- columns get NULL.

-- +goose Up

-- 3 is DEVICE_STATE_REMOVED in protos/delino/ownsand/v1/common.proto. The
-- value is hard-coded because migrations are immutable and independent of
-- the generated Go code; a renumbered enum value would need a new migration.
-- +goose StatementBegin
CREATE FUNCTION scrub_removed_device() RETURNS trigger AS $$
BEGIN
    NEW.name := '';
    NEW.host_os := 0;
    NEW.arch := 0;
    NEW.agent_version := '';
    NEW.update_state := 0;
    NEW.update_target_version := '';
    NEW.pending_idle_since := NULL;
    NEW.capabilities := NULL;
    NEW.limit_vcpu_count := 0;
    NEW.limit_memory_mib := 0;
    NEW.limit_disk_gib := 0;
    NEW.vcpu_overcommit_percent := 100;
    NEW.state_epoch := '';
    NEW.last_seen_time := NULL;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER devices_scrub_removed
    BEFORE INSERT OR UPDATE ON devices
    FOR EACH ROW WHEN (NEW.state = 3)
    EXECUTE FUNCTION scrub_removed_device();

-- Backfill: scrub the tombstones that exist already. The no-op update fires
-- the trigger for each removed row.
UPDATE devices SET state = state WHERE state = 3;
