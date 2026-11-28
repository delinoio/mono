---
title: Minimize Serialization at RSC Boundaries
impact: HIGH
impactDescription: reduces data transfer size
tags: server, rsc, serialization, props
---

## Minimize Serialization at RSC Boundaries

The React Server/Client boundary serializes all object properties into strings and embeds them in the HTML response and subsequent RSC requests. This serialized data directly impacts page weight and load time, so **size matters a lot**. Only pass fields that the client actually uses.

**Incorrect (serializes all 50 fields):**

`page.tsx` (Server Component)

```tsx
import { fetchUser } from './data'
import { Profile } from './profile'

export default async function Page() {
  const user = await fetchUser()  // 50 fields
  return <Profile user={user} />
}
```

`profile.tsx` (Client Component)

```tsx
'use client'

import type { User } from './data'

export function Profile({ user }: { user: User }) {
  return <div>{user.name}</div>  // uses 1 field
}
```

**Correct (serializes only 1 field):**

`page.tsx` (Server Component)

```tsx
import { fetchUser } from './data'
import { Profile } from './profile'

export default async function Page() {
  const user = await fetchUser()
  return <Profile name={user.name} />
}
```

`profile.tsx` (Client Component)

```tsx
'use client'

export function Profile({ name }: { name: string }) {
  return <div>{name}</div>
}
```
