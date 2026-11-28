---
title: PostgreSQL Schema Design
description: Schema design guide
tags: postgres, schema, primary-keys, data-types, foreign-keys, naming
---

# Schema Design

## Primary Keys

Prefer `BIGINT GENERATED ALWAYS AS IDENTITY`. Avoid random UUIDs (UUIDv4) as primary keys. When you need a UUID primary key, use the built-in `uuidv7()` on PostgreSQL 18 or later. [PostgreSQL 18 UUID functions](https://www.postgresql.org/docs/18/functions-uuid.html)

For Mono code that targets PostgreSQL 17, generate the ID in Go with `uuidv7.NewString()` from `go/uuidv7`. Define the column as `id uuid PRIMARY KEY` and supply the generated ID when you insert the row. This follows Mono's UUID v7 default for new persisted entities. [Mono repository defaults](../../../../docs/repository-defaults.md#default-technology-choices) · [PostgreSQL 17 UUID type](https://www.postgresql.org/docs/17/datatype-uuid.html)

```sql
CREATE TABLE user_account (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  email TEXT NOT NULL UNIQUE
);
```

Random UUID PKs (v4) can cause index fragmentation; UUIDs are also larger (16 vs 8 bytes for BIGINT) and can slow joins.

## Data Types

| Use | Avoid |
| --- | --- |
| `TEXT`, `VARCHAR` | Extension-specific types |
| `JSONB` | Custom ENUMs (use CHECK instead) |
| `TIMESTAMPTZ` | `TIMESTAMP` without time zone |
| `BIGINT`, `INTEGER` | Platform-specific types |

Prefer CHECK constraints over ENUM types — they're easier to modify:

```sql
CREATE TABLE customer_order (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  status TEXT NOT NULL CHECK (status IN ('pending', 'shipped', 'delivered'))
);
```

## Foreign Keys

- Always index FK columns (PostgreSQL does not auto-create these)
- Avoid circular FK dependencies
- Suggestion: use `ON DELETE CASCADE` or `ON DELETE SET NULL` explicitly

```sql
CREATE TABLE customer_order (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  customer_id BIGINT NOT NULL REFERENCES customer(id) ON DELETE CASCADE
);
CREATE INDEX customer_order_customer_id_idx ON customer_order (customer_id);
```

## Naming Conventions

- Tables: singular snake_case (`user_account`, `order_item`)
- Columns: singular snake_case (`created_at`, `user_id`)
- Indexes: `{table}_{column}_idx`
- Constraints: `{table}_{column}_{type}` (e.g., `order_status_check`)

## General Guidelines

- Add `NOT NULL` to as many columns as possible
- Add `created_at TIMESTAMPTZ DEFAULT NOW()` to all tables
- Use `BIGINT` for all IDs and foreign keys, even on small tables
- Keep tables normalized; denormalize only for proven hot read paths
