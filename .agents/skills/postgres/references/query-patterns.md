---
title: SQL Query Patterns
description: Common SQL anti-patterns and optimized alternatives
tags: postgres, sql, query-optimization, n-plus-one, pagination
---

# SQL Query Patterns

## Query Structure

**SELECT specific columns** — avoids fetching unnecessary data and enables covering indexes:
```sql
-- Bad:
SELECT * FROM app_user WHERE status = 'active';
-- Good:
SELECT id, name, email FROM app_user WHERE status = 'active';
```

**Subqueries → JOINs** — correlated subqueries re-execute per row:
```sql
-- Bad
SELECT u.id, (SELECT COUNT(*) FROM customer_order o WHERE o.user_id = u.id) FROM app_user u;
-- Good
SELECT u.id, COUNT(o.id) FROM app_user u LEFT JOIN customer_order o ON o.user_id = u.id GROUP BY u.id;
```

**Always LIMIT unbounded queries** — prevent runaway result sets:
```sql
SELECT id, message FROM log WHERE level = 'error' ORDER BY created_at DESC LIMIT 100;
```

**Avoid functions on indexed columns (SARGable)** — functions prevent index usage unless a functional index exists:
```sql
-- Bad: Full table scan
SELECT * FROM app_user WHERE date_trunc('day', created_at) = '2023-01-01';
-- Good: Index scan
SELECT * FROM app_user WHERE created_at >= '2023-01-01' AND created_at < '2023-01-02';
```

## N+1 Detection

**Batch queries with `ANY` in Psycopg 3**

With the default cursor, pass a Python list as the single query parameter.
Psycopg binds the list as a PostgreSQL array. An empty list matches no rows.

```python
# Bad: one query per ID
for uid in user_ids:
    cursor.execute("SELECT name FROM app_user WHERE id = %s", (uid,))

# Good: one query for all IDs
cursor.execute("SELECT id, name FROM app_user WHERE id = ANY(%s)", (list(user_ids),))
```

**ORM lazy loading → eager loading:**
```python
# Bad: N+1 — each iteration fires a query
for user in User.query.all():
    print(user.posts)
# Good
users = User.query.options(joinedload(User.posts)).all()
```

## Query Rewrites

**UNION → UNION ALL** — skip deduplication when duplicates are impossible or acceptable.

**IN subquery → EXISTS** — EXISTS short-circuits on first match:
```sql
SELECT id, name FROM app_user u
WHERE EXISTS (SELECT 1 FROM customer_order o WHERE o.user_id = u.id AND o.total > 100);
```

**OFFSET → cursor pagination** — OFFSET scans and discards rows, degrading at depth:
```sql
-- Bad: OFFSET 10000 scans 10020 rows
SELECT id, title FROM article ORDER BY created_at DESC LIMIT 20 OFFSET 10000;
-- Good: cursor-based (requires index on (created_at DESC, id DESC))
SELECT id, title FROM article
WHERE (created_at, id) < ('2025-06-15T12:00:00Z', 987654)
ORDER BY created_at DESC, id DESC LIMIT 20;
```
