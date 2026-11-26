---
title: MVCC Transactions and Concurrency
description: Transaction isolation levels, XID wraparound prevention, serialization errors, and long-transaction impact
tags: postgres, mvcc, transactions, isolation, xid-wraparound, concurrency, serialization
---

# MVCC Transactions and Concurrency

## Transaction Isolation Levels

- **READ UNCOMMITTED** — treated as READ COMMITTED in PostgreSQL; no dirty reads ever.
- **READ COMMITTED** (default): new snapshot per statement; can see different data within same tx.
- **REPEATABLE READ**: snapshot at first query; can cause serialization errors on write conflicts.
- **SERIALIZABLE**: strongest; transactions appear serial; requires retry logic in app code.

Ordinary snapshot reads (plain `SELECT`) and ordinary DML do not block each other through their normal row-data locks; only writers that touch the same row conflict. Locking reads such as `SELECT ... FOR UPDATE` and writes (or other lockers) can wait on each other for conflicting row locks on the same row, and a plain `SELECT` takes `ACCESS SHARE` on the table, so it waits behind a conflicting `ACCESS EXCLUSIVE` table lock (for example from DDL). No lock escalation — row locks never degrade to table locks.

## XID Wraparound

32-bit transaction IDs wrap at ~2 billion (2^31). `VACUUM FREEZE` replaces old XIDs with FrozenXID (value 2, always visible). Without freeze: after wraparound, old rows appear "in the future" and become **invisible**. Data physically exists but is invisible to all queries — looks like total data loss. XID wraparound should be avoided at all cost.

Warning messages start at ~1.4B XIDs. **Keep autovacuum enabled and properly configured** to protect the database against wraparound.

PostgreSQL 14 and later refuse to allocate new XIDs near the wraparound limit. Keep the server running and follow the recovery procedure for your version, even if the PostgreSQL 14 error hint says to stop the server.

1. Resolve blockers first. Check for old prepared transactions, long-running transactions and obsolete replication slots that hold back vacuum. Follow the steps in the manual for your version. [PostgreSQL 14](https://www.postgresql.org/docs/14/routine-vacuuming.html#VACUUM-FOR-WRAPAROUND) · [PostgreSQL 18](https://www.postgresql.org/docs/18/routine-vacuuming.html#VACUUM-FOR-WRAPAROUND)
2. Run ordinary database-wide `VACUUM` in the affected database as a superuser so it can process the system catalogs. Avoid `VACUUM FULL` because it needs a new XID. Avoid `VACUUM FREEZE` because it does more work than recovery requires.

On PostgreSQL 14 and later, use single-user mode only if you deliberately need to `DROP` or `TRUNCATE` unneeded tables during recovery. Single-user mode requires downtime and disables the XID wraparound safeguards.

PostgreSQL 13 required stopping the server and running `VACUUM` in single-user mode after a wraparound safety shutdown. [PostgreSQL 13](https://www.postgresql.org/docs/13/routine-vacuuming.html#VACUUM-FOR-WRAPAROUND)

## XID Age Monitoring

```sql
SELECT datname, age(datfrozenxid),
  ROUND(100.0 * age(datfrozenxid) / 2147483648, 2) AS pct
FROM pg_database ORDER BY age(datfrozenxid) DESC;
```

## Long Transaction Impact

A single long-running transaction blocks VACUUM from removing dead tuples across the **entire database**. Causes table bloat, increased disk, slower queries, cache pollution. `idle_in_transaction` connections are the #1 operational MVCC issue. Set `idle_in_transaction_session_timeout` (30s–5min). Dead tuples waste I/O on seq scans and cause useless heap lookups from indexes.

## Serialization Errors

Apps **must** handle "could not serialize access" with retry logic. More common in REPEATABLE READ and SERIALIZABLE. Smaller, faster transactions reduce conflict frequency.
