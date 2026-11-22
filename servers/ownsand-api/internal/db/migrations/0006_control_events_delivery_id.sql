-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Delivery order of control events. event_id is allocated at insert time,
-- before the transaction commits, so a lower event_id can become visible
-- after a higher one and a keyset poll on event_id skips it. delivery_id
-- is assigned at the end of the transaction, in commit order:
--
--   * A deferred constraint trigger runs at commit time, takes the
--     transaction-scoped advisory lock 0x6f776e5f6374 ("own_ct"), and only
--     then takes the next value of control_events_delivery_id_seq. Postgres
--     releases a transaction-scoped advisory lock after the transaction is
--     visible to other sessions, so a reader that sees delivery_id N + 1
--     also sees N. Transactions that write events therefore commit one at a
--     time. A transaction that writes several events takes the same lock
--     again for each of them.
--   * The sequence has CACHE 1 so that a value is never taken ahead of the
--     lock holder.
--   * The trigger is a database trigger, so the INSERT statement of an older
--     release (which only knows event_id) gets a delivery_id as well while
--     the old and the new container run together during a deployment. The
--     old release keeps polling by event_id until it stops.
--   * delivery_id stays NULL until the trigger runs and is never NULL in a
--     committed row, so it has no NOT NULL constraint (the insert happens
--     before the trigger).
--
-- The advisory lock key must differ from the migration runner's lock key
-- (0x6f776e73616e64) because both use the bigint advisory lock namespace.

-- +goose Up

ALTER TABLE control_events ADD COLUMN delivery_id bigint;

UPDATE control_events SET delivery_id = event_id;

CREATE SEQUENCE control_events_delivery_id_seq CACHE 1;
-- The next delivery_id is above every backfilled value.
SELECT setval('control_events_delivery_id_seq', COALESCE(max(delivery_id), 0) + 1, false) FROM control_events;

CREATE UNIQUE INDEX control_events_delivery_id_idx ON control_events (delivery_id);

-- +goose StatementBegin
CREATE FUNCTION control_events_assign_delivery_id() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_advisory_xact_lock(x'6f776e5f6374'::bigint);
    UPDATE control_events SET delivery_id = nextval('control_events_delivery_id_seq')
        WHERE event_id = NEW.event_id;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER control_events_assign_delivery_id
    AFTER INSERT ON control_events
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION control_events_assign_delivery_id();
