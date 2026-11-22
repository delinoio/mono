-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Requests and runners. Enum columns store the protobuf enum numbers of
-- delino.tide.v1; `source` stores work.Source. Requests and runners keep the
-- runner profile ID without a foreign key, because ended rows stay for 30
-- days after their profile is deleted. A trigger gives every insert and
-- update a new change_seq, which WatchQueue follows. The owning contract is
-- docs/servers-tide-api-foundation.md (Queue and dispatch, Runner lifecycle,
-- Dev sessions, Storage).

-- +goose Up
CREATE SEQUENCE work_change_seq;

-- +goose StatementBegin
CREATE FUNCTION work_touch() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.change_seq := nextval('work_change_seq');
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TABLE requests (
    request_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    runner_profile_id uuid NOT NULL,
    -- tidev1.WorkloadKind.
    kind smallint NOT NULL,
    -- work.Source: who sent the demand.
    source smallint NOT NULL,
    -- The GitHub job ID or vendor request key; the request ID for dev
    -- sessions. A repeated event or claim cannot create a second request.
    source_key text NOT NULL,
    -- tidev1.RequestState and tidev1.RequestEndReason.
    state smallint NOT NULL,
    end_reason smallint NOT NULL DEFAULT 0,
    -- tidev1.Priority, copied from the profile when the request arrives.
    priority smallint NOT NULL,
    -- The original queue time; a retry after a start failure keeps it.
    queue_time timestamptz NOT NULL,
    queue_deadline timestamptz NOT NULL,
    start_time timestamptz,
    end_time timestamptz,
    runner_id uuid,
    start_attempts integer NOT NULL DEFAULT 0,
    -- DEV_SESSION: the session owner (Logto `sub`) and its label.
    owner_user_id text NOT NULL DEFAULT '',
    label text NOT NULL DEFAULT '',
    -- The runner pool of a request whose runners the source binds
    -- (GITHUB_JOB: the GitHub installation); NULL otherwise.
    pool_key text,
    -- The encrypted Claude Code work order of a queued request (later
    -- layer); deleted when the runner is BUSY or the request ends.
    work_order_key_id text,
    work_order_ciphertext bytea,
    create_time timestamptz NOT NULL,
    update_time timestamptz NOT NULL,
    change_seq bigint NOT NULL,
    UNIQUE (organization_id, source, source_key)
);
CREATE INDEX requests_queue_idx ON requests (organization_id, priority, queue_time, request_id) WHERE state = 1;
CREATE INDEX requests_open_idx ON requests (organization_id, state) WHERE state IN (1, 2, 3);
CREATE INDEX requests_owner_idx ON requests (organization_id, owner_user_id, state) WHERE kind = 3;
CREATE INDEX requests_change_idx ON requests (organization_id, change_seq);
CREATE INDEX requests_end_idx ON requests (end_time) WHERE end_time IS NOT NULL;
CREATE TRIGGER requests_touch BEFORE INSERT OR UPDATE ON requests FOR EACH ROW EXECUTE FUNCTION work_touch();

CREATE TABLE runners (
    runner_id uuid PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES organizations ON DELETE CASCADE,
    runner_profile_id uuid NOT NULL,
    kind smallint NOT NULL,
    guest_os smallint NOT NULL,
    -- tidev1.RunnerState and tidev1.RunnerEndReason.
    state smallint NOT NULL,
    end_reason smallint NOT NULL DEFAULT 0,
    request_id uuid,
    pool_key text,
    -- Stored before CreateSandbox, so a retry after a lost response does
    -- not create a second sandbox.
    idempotency_key uuid NOT NULL UNIQUE,
    -- The CreateSandbox request (protobuf encoding) that the first call
    -- sent with the key. Every retry sends it unchanged, because OwnSand
    -- binds the key to the request parameters and a later profile change
    -- would fail the retry with already_exists.
    create_request bytea NOT NULL,
    ownsand_sandbox_id uuid,
    ownsand_device_id text NOT NULL DEFAULT '',
    ownsand_end_reason text NOT NULL DEFAULT '',
    -- Copied from the profile at creation: a later profile change applies
    -- only to runners that start after it.
    workspace_path text NOT NULL DEFAULT '',
    -- The exec of the runner program that tide follows with AttachExec.
    program_exec_id uuid,
    create_time timestamptz NOT NULL,
    ready_time timestamptz,
    end_time timestamptz,
    update_time timestamptz NOT NULL,
    change_seq bigint NOT NULL
);
CREATE INDEX runners_open_idx ON runners (organization_id, runner_profile_id, state) WHERE state NOT IN (7, 8);
CREATE UNIQUE INDEX runners_request_idx ON runners (request_id) WHERE request_id IS NOT NULL AND state NOT IN (7, 8);
CREATE INDEX runners_change_idx ON runners (organization_id, change_seq);
CREATE INDEX runners_end_idx ON runners (end_time) WHERE end_time IS NOT NULL;
CREATE TRIGGER runners_touch BEFORE INSERT OR UPDATE ON runners FOR EACH ROW EXECUTE FUNCTION work_touch();
