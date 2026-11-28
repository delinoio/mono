---
title: PgBouncer Configuration
description: Pool sizing and connection limits
tags: postgres, pgbouncer, connection-pooling, configuration
---

# PgBouncer Configuration

## default_pool_size

Server connections per user/database pair. **Default: 20**

When every active pool inherits the same positive `default_pool_size`, multiply it by the number of active pools to get the base total. For example, 2 users with a pool on each of 3 database entries have 6 pools. With a default of 45, their base total is 270 server connections before reserves and caps.

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

Calculate a separate server connection bound for each database entry on each PgBouncer instance.

1. For each active pool, take the base size from the first configured setting in this order. Check `pool_size` in `[users]`, then `pool_size` in `[databases]`, then the global `default_pool_size`.
2. Resolve `reserve_pool_size` in the same order. Use the setting from `[users]`, then `[databases]`, then the global value. Add the effective reserve size to the pool's base size if both the size and `reserve_pool_timeout` are greater than zero.
3. Sum the base sizes and enabled reserves across the entry's active pools. Use a positive `max_db_connections` from `[databases]` when set, or use the global setting otherwise. If the effective limit is positive, use the smaller of that limit and the sum. If the effective limit is zero, use the sum.

An effective base size of zero leaves that pool without a pool-size limit. For an entry with such a pool, use the effective `max_db_connections` if it is positive. The pool sizes alone give no finite bound for that entry.

Add the bounds for all entries that target the same PostgreSQL server, including aliases of the same database and entries on different PgBouncer instances. The total must fit within the server's pooled budget.

See the PgBouncer settings for [user pool sizes](https://www.pgbouncer.org/config.html#pool_size-1), [user reserve sizes](https://www.pgbouncer.org/config.html#reserve_pool_size-2), and [database connection limits](https://www.pgbouncer.org/config.html#max_db_connections).

View: `SHOW max_connections;`

## Examples

Single database/user: `default_pool_size = 45, max_user_connections = 0`

- **Database override.** With one active user, `default_pool_size = 20`, and database `pool_size = 70`, the entry contributes 70 server connections. The user inherits the database size, reserves are disabled, and the database and user limits are unlimited. With `max_connections = 100`, 30 direct connections, and a 20-connection buffer, the pooled budget is 100 − 30 − 20 = 50. The entry exceeds that budget by 20 connections.
- **User override.** With two active users, `default_pool_size = 20`, and database `pool_size = 30`, one user's `pool_size = 70` takes precedence. With reserves disabled and no connection caps, the entry contributes 70 + 30 = 100 connections.
- **Enabled reserves.** With two active users, no overrides or caps, `default_pool_size = 20`, `reserve_pool_size = 5`, and a positive `reserve_pool_timeout`, the entry contributes 2 × (20 + 5) = 50 connections.
- **Database cap.** With two active users inheriting database `pool_size = 70` and reserves disabled, the pool sizes sum to 2 × 70 = 140. With `max_db_connections = 50`, the entry contributes at most 50 connections.

Multiple users/databases: one PgBouncer instance with three database entries on one PostgreSQL server, `max_db_connections = 50` on each entry, `default_pool_size = 25`, `reserve_pool_size = 0`, two active users. Each entry opens at most 50 connections, so the pooled maximum is 3 × 50 = 150 (2 users × `default_pool_size` 25 also gives 50 per entry, so the cap matches the pool sizes). With `max_connections = 200`, 10 direct connections and a 40-connection buffer (20% of 200), the budget is 200 − 10 − 40 = 150, so 150 + 10 + 40 = 200 fits exactly. A fourth entry with the same cap raises the pooled maximum to 200, which is over budget. Per-user `max_user_connections` caps are limits within this budget, not additions to it.

## Monitoring

```sql
SELECT datname, usename, COUNT(*) FROM pg_stat_activity WHERE backend_type = 'client backend' GROUP BY datname, usename;
```

