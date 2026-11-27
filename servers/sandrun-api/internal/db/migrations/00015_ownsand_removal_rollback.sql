-- SPDX-License-Identifier: FSL-1.1-ALv2

-- +goose Up
CREATE TABLE ownsand_removal_rollback (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    deployment_id UUID
);

INSERT INTO ownsand_removal_rollback DEFAULT VALUES;

-- +goose StatementBegin
CREATE FUNCTION check_ownsand_removal_admission() RETURNS VOID
LANGUAGE plpgsql AS $$
DECLARE
    admitted BOOLEAN;
BEGIN
    SELECT deployment_id IS NULL INTO admitted
    FROM ownsand_removal_rollback WHERE singleton FOR SHARE NOWAIT;
    IF admitted IS TRUE THEN
        RETURN;
    END IF;
    RAISE EXCEPTION USING ERRCODE = 'SR001', MESSAGE = 'OwnSand teardown is paused for rollback';
EXCEPTION
    WHEN lock_not_available THEN
        RAISE EXCEPTION USING ERRCODE = 'SR001', MESSAGE = 'OwnSand teardown is paused for rollback';
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION fence_ownsand_removal_claim() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.removal_token IS NOT NULL
        AND (TG_OP = 'INSERT' OR NEW.removal_token IS DISTINCT FROM OLD.removal_token) THEN
        PERFORM check_ownsand_removal_admission();
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- Keep the shared lock through claim commit. This also covers lease-aware
-- binaries that predate the RPC admission check.
CREATE TRIGGER ownsand_removal_claim_admission
    BEFORE INSERT OR UPDATE OF removal_token ON ownsand_links
    FOR EACH ROW EXECUTE FUNCTION fence_ownsand_removal_claim();
