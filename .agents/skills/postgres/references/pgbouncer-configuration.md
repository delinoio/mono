---
title: PgBouncer Configuration
description: Pool sizing and connection limits
tags: postgres, pgbouncer, connection-pooling, configuration
---

# PgBouncer Configuration

## default_pool_size

Server connections per user/database pair. **Default: 20**

**Multiplication:** 2 users × 3 databases = `6 × default_pool_size` connections  
**Example:** 45 with 2 users and 3 databases = 270 backend connections

**Recommended values:**
- 1 or few database/user pairs OLTP: `25-50`
- High # of database/user pairs active simultaneously: `10-25`

## max_user_connections

Max backend connections per PgBouncer user across all database entries. Set in the `[users]` section or globally. **Default: 0 (unlimited)**

This is a per-user limit, not a pooled budget for the server. A per-user cap limits one user within the pooled budget (see [Pooled connection budget](#pooled-connection-budget)); it does not replace the budget calculation.

## max_db_connections

Max backend connections per PgBouncer database entry (`[databases]` section), regardless of user. **Default: 0 (unlimited)**

Each entry counts separately, including entries that are aliases of the same PostgreSQL database. Use it to bound what one entry can add to the pooled budget.

## Postgres max_connections

Max concurrent connections to Postgres. **Default: 100**. Setting requires restart.

### Pooled connection budget

Compute the budget per PostgreSQL server:

`pooled budget = max_connections − anticipated steady-state direct connections − buffer` (buffer = 20% of `max_connections`)

The sum of the maximum server connections that all PgBouncer instances can open to that server must fit in the pooled budget. Count every PgBouncer instance, every database entry (including aliases) and every enabled reserve pool (`reserve_pool_size`) that targets the server. For each entry the maximum is bounded by `max_db_connections` when set, otherwise by `default_pool_size × active users` (plus `reserve_pool_size` per pool when enabled).

View: `SHOW max_connections;`

## Examples

Single database/user: `default_pool_size = 45, max_user_connections = 0`

Multiple users/databases: one PgBouncer instance with three database entries on one PostgreSQL server, `max_db_connections = 50` on each entry, `default_pool_size = 25`, `reserve_pool_size = 0`, two active users. Each entry opens at most 50 connections, so the pooled maximum is 3 × 50 = 150 (2 users × `default_pool_size` 25 also gives 50 per entry, so the cap matches the pool sizes). With `max_connections = 200`, 10 direct connections and a 40-connection buffer (20% of 200), the budget is 200 − 10 − 40 = 150, so 150 + 10 + 40 = 200 fits exactly. A fourth entry with the same cap raises the pooled maximum to 200, which is over budget. Per-user `max_user_connections` caps are limits within this budget, not additions to it.

## Monitoring

```sql
SELECT datname, usename, COUNT(*) FROM pg_stat_activity WHERE backend_type = 'client backend' GROUP BY datname, usename;
```

