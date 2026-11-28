---
title: Avoid Duplicate Serialization in RSC Props
impact: LOW
impactDescription: reduces network payload by avoiding duplicate serialization
tags: server, rsc, serialization, props, client-components
---

## Avoid Duplicate Serialization in RSC Props

**Impact: LOW (reduces network payload by avoiding duplicate serialization)**

RSC→client serialization deduplicates by object reference, not value. Same reference = serialized once; new reference = serialized again. Do transformations (`.toSorted()`, `.filter()`, `.map()`) in client, not server.

**Incorrect (duplicates array):**

```tsx
// RSC: sends 6 strings (2 arrays × 3 items)
<ClientList usernames={usernames} usernamesOrdered={usernames.toSorted()} />
```

**Correct (sends 3 strings):**

```tsx
// UserList.tsx (Server Component)
import { ClientList } from './ClientList'

export default function UserList() {
  const usernames = ['zoe', 'amy', 'mika']
  return <ClientList usernames={usernames} />
}
```

```tsx
// ClientList.tsx (Client Component)
'use client'

import { useMemo } from 'react'

export function ClientList({ usernames }: { usernames: string[] }) {
  const sorted = useMemo(() => [...usernames].sort(), [usernames])

  return (
    <ol>
      {sorted.map(username => (
        <li key={username}>{username}</li>
      ))}
    </ol>
  )
}
```

**Nested deduplication behavior:**

Deduplication works recursively. Impact varies by data type:

- `string[]`, `number[]`, `boolean[]`: **HIGH impact** - array + all primitives fully duplicated
- `object[]`: **LOW impact** - array duplicated, but nested objects deduplicated by reference

```tsx
// string[] - duplicates everything
usernames={['a','b']} sorted={usernames.toSorted()} // sends 4 strings

// object[] - the two arrays share the same two objects
function UserList() {
  const users = [{ id: 2 }, { id: 1 }]
  const sorted = users.toSorted((a, b) => a.id - b.id)
  return <ClientList users={users} sorted={sorted} />
}
```

**Operations breaking deduplication (create new references):**

- Arrays: `.toSorted()`, `.filter()`, `.map()`, `.slice()`, `[...arr]`
- Objects: `{...obj}`, `Object.assign()`, `structuredClone()`, `JSON.parse(JSON.stringify())`

**More examples:**

```tsx
// ❌ Bad
<C users={users} active={users.filter(u => u.active)} />
<C product={product} productName={product.name} />

// ✅ Good
<C users={users} />
<C product={product} />
// Do filtering/destructuring in client
```

**Exception:** Pass derived data when transformation is expensive or client doesn't need original.
