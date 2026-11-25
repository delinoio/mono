-- SPDX-License-Identifier: FSL-1.1-ALv2

-- +goose Up
-- Rows from before this migration get a transaction ID on their next write.
ALTER TABLE requests ADD COLUMN watch_xact_id xid8;
ALTER TABLE runners ADD COLUMN watch_xact_id xid8;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION work_touch() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.change_seq := nextval('work_change_seq');
    NEW.watch_xact_id := pg_current_xact_id();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE INDEX requests_watch_xact_idx ON requests (organization_id, watch_xact_id) WHERE watch_xact_id IS NOT NULL;
CREATE INDEX runners_watch_xact_idx ON runners (organization_id, watch_xact_id) WHERE watch_xact_id IS NOT NULL;
