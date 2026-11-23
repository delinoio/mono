---
title: Build Index Maps for Repeated Lookups
impact: LOW-MEDIUM
impactDescription: 1M ops to 2K ops
tags: javascript, map, indexing, optimization, performance
---

## Build Index Maps for Repeated Lookups

Multiple `.find()` calls by the same key should use a Map.

**Incorrect (O(n) per lookup):**

```typescript
function processOrders(orders: Order[], users: User[]) {
  return orders.map(order => ({
    ...order,
    user: users.find(u => u.id === order.userId)
  }))
}
```

**Correct (O(1) per lookup):**

```typescript
function processOrders(orders: Order[], users: User[]) {
  // IDs are strings. Keep the first user per ID, like users.find() does
  const userById = new Map<string, User>()
  for (const user of users) {
    if (!userById.has(user.id)) userById.set(user.id, user)
  }

  return orders.map(order => ({
    ...order,
    user: userById.get(order.userId)
  }))
}
```

Build map once (O(n)), then all lookups are O(1). Inserting only when the ID is absent keeps the first match for a duplicate ID, which is what `.find()` returns; `new Map(users.map(u => [u.id, u]))` would keep the last one.
For 1000 orders × 1000 users: 1M ops → 2K ops.
