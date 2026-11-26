---
title: WAL and Checkpoint Operations
description: Write-ahead log internals, checkpoint tuning, durability guarantees, and WAL disk management
tags: postgres, wal, checkpoints, durability, crash-recovery, fsync, operations
---

# WAL and Checkpoint Operations

## WAL Fundamentals

Write-ahead logging flushes WAL records in `pg_wal/` before PostgreSQL writes the corresponding changes to data files. WAL segments are 16MB by default. At cluster initialization, `initdb --wal-segsize` can set another power-of-two size from 1MB to 1024MB. PostgreSQL can write WAL records for transactions that have yet to commit or that later roll back.

For an ordinary WAL-logged transaction with `fsync` enabled and the default `synchronous_commit = on`, PostgreSQL waits for the local WAL flush before it reports commit success. PostgreSQL can write the modified data pages later.

With `synchronous_commit = off` and `fsync` enabled, PostgreSQL can report commit success before the local WAL flush. If PostgreSQL crashes before that flush, it can lose recent acknowledged commits. With normal crash-safety settings, PostgreSQL replays WAL up to the last flushed record and recovers to a consistent state. Certain operations, including two-phase commit commands, still commit synchronously. [PostgreSQL WAL settings](https://www.postgresql.org/docs/current/runtime-config-wal.html#GUC-SYNCHRONOUS-COMMIT) · [Asynchronous commit](https://www.postgresql.org/docs/current/wal-async-commit.html)

**Never disable `fsync` in production.** Power loss with `fsync` disabled can cause unrecoverable data loss.

`wal_level`: `minimal` (crash recovery only), `replica` (default; replication + archiving), `logical` (logical replication).

## Dirty Pages and Checkpoints

A dirty page is modified in shared_buffers but not yet written to data files. A checkpoint flushes all dirty pages to disk and writes a checkpoint record to WAL; recovery only replays WAL since the last checkpoint.

- `checkpoint_timeout` (default 5 min) and `max_wal_size` (default 1GB) — checkpoint on whichever triggers first.
- `checkpoint_completion_target=0.9` spreads I/O over 90% of the interval; avoid spikes.
- In `pg_stat_checkpointer`, `num_timed` counts scheduled checkpoints. `num_requested` counts requested checkpoints, including WAL-size triggers and manual SQL `CHECKPOINT` commands.
- Consider increasing `max_wal_size` only when you have evidence of frequent WAL-triggered checkpoints, such as the PostgreSQL log message "checkpoints are occurring too frequently". Use the logs to identify the cause when `num_requested` rises.

## WAL Disk Management

Replication slots prevent WAL deletion even when standbys are offline — they can fill disk. WAL archiving failures also block recycling. `max_wal_size` is a *soft* limit; WAL can grow beyond it under heavy load.

WAL size: `SELECT count(*) AS files, pg_size_pretty(sum(size)) AS total FROM pg_ls_waldir();`

Slot lag: `SELECT slot_name, pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn) AS lag_bytes FROM pg_replication_slots;`

## Checkpoint Monitoring

PG17+ moved checkpoint stats from `pg_stat_bgwriter` to `pg_stat_checkpointer` and renamed columns.

`SELECT num_timed, num_requested, write_time, sync_time, buffers_written FROM pg_stat_checkpointer;`

Backend-direct writes (formerly `buffers_backend` in `pg_stat_bgwriter`) are now tracked in `pg_stat_io`: `SELECT writes FROM pg_stat_io WHERE backend_type = 'client backend' AND object = 'relation';`

## Crash Recovery

On crash, PostgreSQL replays WAL from the last checkpoint. Longer checkpoint intervals → more WAL to replay → longer recovery. Trade-off: frequent checkpoints (faster recovery, more I/O) vs infrequent (less I/O, slower recovery). For most workloads, `checkpoint_timeout=5min` and `max_wal_size` tuned to keep checkpoints time-based is the right balance.
