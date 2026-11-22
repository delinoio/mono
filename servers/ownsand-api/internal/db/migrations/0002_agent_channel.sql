-- SPDX-License-Identifier: FSL-1.1-ALv2
--
-- Agent channel support: the instance lease. Only the lease holder accepts
-- agent channels and runs heartbeat timers and state transitions.

-- +goose Up

CREATE TABLE instance_leases (
    lease_name text PRIMARY KEY,
    holder text NOT NULL,
    expire_time timestamptz NOT NULL
);
