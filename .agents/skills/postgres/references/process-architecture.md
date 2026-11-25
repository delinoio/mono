---
title: Process Architecture
description: PostgreSQL multi-process model, connection management, and auxiliary processes
tags: postgres, processes, connections, pooling, memory, operations
---

# Process Architecture

PostgreSQL uses a **multi-process** model, not multi-threaded: one OS process per client connection. The postmaster is the parent; it spawns backend processes per connection. Each backend has some private memory (`work_mem`, temp buffers). 1000 connections = 1000 processes (~5–10MB base + query memory each). There is also a large buffer shared amongst all.

## Auxiliary Processes

WAL Writer, Background Writer, Checkpointer, Autovacuum Launcher/Workers, Archiver, WAL Summarizer (PG 17+). These run alongside backends and are not spawned per connection.

## Memory Risk

`work_mem` is per-operation, not per-query. Estimate: `work_mem × operations_per_query × parallel_workers × connections` can grow very large at high concurrency. Scale connections and parallelism before raising `work_mem`.

## Connection Pooling (Critical)

Each connection = OS process (fork overhead, context switching, memory). PgBouncer can multiplex many app connections to fewer DB connections. Typical: 1000 app connections → pooler → 20–50 backends. Implement pooling before raising `max_connections`; `max_connections` requires a full restart to change (default 100).

The `superuser_reserved_connections` setting (default 3) reserves the last available slots for superusers only. PostgreSQL 16 and later also provide `reserved_connections` (default 0). Superusers and roles with privileges of `pg_use_reserved_connections` can use that reserve before the final superuser-only reserve. An ordinary role has neither privilege.

## Monitoring

```sql
SELECT state, count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' GROUP BY state;
```

```sql
WITH client_backends AS (
    SELECT count(*) AS used
    FROM pg_stat_activity
    WHERE backend_type = 'client backend'
)
SELECT used,
       GREATEST(0,
           current_setting('max_connections')::integer
           - current_setting('superuser_reserved_connections')::integer
           - COALESCE(current_setting('reserved_connections', true)::integer, 0)
           - used
       ) AS ordinary_role_headroom
FROM client_backends;
```

`ordinary_role_headroom` estimates how many more ordinary-role connections the server can accept based on the sampled client-backend count. If the server has no `reserved_connections` setting, the query treats that reserve as zero. Database and role connection limits can reduce actual capacity further.

Use `pg_activity` for interactive top-like monitoring. Alert at 80% connection usage, critical at 95%. Count by state to find idle-in-transaction leaks — these hold locks and **block VACUUM** from reclaiming dead tuples.

## Common Problems

| Problem | Fix |
| ------- | --- |
| `too many clients already` | Implement pooling; find idle connections; check for connection leaks |
| High memory / OOM | Reduce `work_mem`; add pooling; set `statement_timeout` |
| Stuck process | `SELECT pg_cancel_backend(pid);` then `SELECT pg_terminate_backend(pid);` — **always confirm with a human before terminating backends**, as this may abort in-flight transactions and cause data issues for the application |

Prefer pooling + conservative `max_connections` over raising limits reactively.
