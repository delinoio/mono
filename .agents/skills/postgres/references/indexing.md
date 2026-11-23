---
title: Indexing Best Practices
description: Index design guide
tags: postgres, indexes, composite, partial, covering, gin, brin
---

# Indexing Best Practices

## Core Rules

1. **Always index foreign key columns** — PostgreSQL does not auto-create these
2. **Index columns in WHERE, JOIN, and ORDER BY** clauses
3. **Don't over-index** — each index slows writes and uses storage
4. **Verify with EXPLAIN ANALYZE** — confirm indexes are actually used

## Composite Indexes

Put equality columns first, then range/sort columns:

```sql
-- WHERE status = 'active' AND created_at > '2026-01-01'
CREATE INDEX customer_order_status_created_idx ON customer_order (status, created_at);
```

A composite B-tree index on `(a, b)` is most efficient for filters on `a`, or on `a` and `b`. A filter on `b` alone can still use the index, usually less efficiently: PostgreSQL scans the whole index, which the planner rarely prefers over a sequential scan. PostgreSQL 18 can use a skip scan when the planner estimates it is cheaper, typically when `a` has few distinct values; the planner decides on cost. Check the plan with `EXPLAIN ANALYZE` before adding a separate index on `b`.

## Partial Indexes

Reduce index size by filtering to common query patterns.
Only use if index size is problematic but the index is needed for performance.

```sql
CREATE INDEX customer_order_active_idx ON customer_order (customer_id)
  WHERE status = 'active';
```

## Covering Indexes

Consider creating covering indexes for commonly executed query patterns that return only 1 or a small number of columns.

## Index Types

| Type | Use Case | Example |
| --- | --- | --- |
| B-tree (default) | Equality, range, sorting | `WHERE id = 1`, `ORDER BY date` |
| GIN | Arrays, JSONB, full-text | `WHERE tags @> ARRAY['x']` |
| GiST | Geometric, range types, full-text | PostGIS, `tsrange`, `tsvector` |
| BRIN | Large sequential/time-series | Append-only logs, events (requires physical row order correlation) |

```sql
CREATE INDEX metadata_idx ON customer_order USING GIN (metadata);       -- JSONB
CREATE INDEX event_created_idx ON event USING BRIN (created_at); -- time-series
```

## Guidelines

- Name indexes consistently: `{table}_{column}_idx`
- Review for unused indexes periodically
- **Always confirm with a human before removing or dropping any indexes** — even unused ones may serve a purpose not reflected in recent stats
- Use partial indexes for frequently filtered subsets
- Use covering indexes on hot read paths
