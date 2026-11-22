-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Exact sandbox TTLs. sandboxes.idle_ttl_ms and sandboxes.max_ttl_ms hold
-- the TTL in whole milliseconds, but the API accepts every positive
-- duration, so a TTL below one millisecond (or with a fraction of a
-- millisecond) was stored truncated and the sandbox that GetSandbox and an
-- idempotent CreateSandbox replay returned no longer matched the TTL that
-- the create command carried to the agent. The owning contract is the TTL
-- section of docs/servers-ownsand-api-foundation.md.
--
-- The new columns hold the TTL in nanoseconds, the precision of a Go
-- duration and of the protobuf Duration the API returns:
--   idle_ttl_ns, max_ttl_ns   NULL for a row that an earlier release wrote;
--                             the readers then use the millisecond column
--                             (value * 1000000), so a stored whole-
--                             millisecond TTL keeps its value.
-- Both columns are nullable and the millisecond columns stay NOT NULL and
-- are still written (truncated), so the previous release, which keeps
-- serving during a rolling deployment, inserts and reads rows with its old
-- column list. Remove the millisecond columns only in a later migration
-- after no release reads them.

-- +goose Up

ALTER TABLE sandboxes
    ADD COLUMN idle_ttl_ns bigint,
    ADD COLUMN max_ttl_ns bigint;
