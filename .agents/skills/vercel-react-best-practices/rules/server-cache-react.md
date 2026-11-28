---
title: Per-Request Deduplication with React.cache()
impact: MEDIUM
impactDescription: deduplicates within request
tags: server, cache, react-cache, deduplication
---

## Per-Request Deduplication with React.cache()

Use `React.cache()` for server-side request deduplication. Authentication and database queries benefit most.

Define each memoized wrapper once at module scope. Call it during a React Server Component render. Calls to the same wrapper with matching arguments share a result within that render request. At module scope, each call executes the underlying function.

```typescript
import { cache } from 'react'

export const getCurrentUser = cache(async () => {
  const session = await auth()
  if (!session?.user?.id) return null
  return await db.user.findUnique({
    where: { id: session.user.id }
  })
})

export async function CurrentUser() {
  await Promise.all([getCurrentUser(), getCurrentUser()]) // One query if signed in
  return null
}
```

**Argument identity**

Primitive arguments match by value. Object arguments match by reference. Reuse the same object to share the result. Two object literals with the same fields create separate cache entries.

```typescript
import { cache } from 'react'

const getUser = cache(async (uid: number) => {
  return await db.user.findUnique({ where: { id: uid } })
})

const getUserByParams = cache(async (params: { uid: number }) => {
  return await db.user.findUnique({ where: { id: params.uid } })
})

export async function PrimitiveArgument() {
  await Promise.all([getUser(1), getUser(1)]) // One query
  return null
}

export async function SharedObjectArgument() {
  const params = { uid: 1 }
  await Promise.all([getUserByParams(params), getUserByParams(params)]) // One query
  return null
}

export async function FreshObjectArguments() {
  await Promise.all([getUserByParams({ uid: 1 }), getUserByParams({ uid: 1 })]) // Two queries
  return null
}
```

**Next.js-Specific Note:**

In Next.js 15, React request memoization applies only to `GET` calls to `fetch`. Calls with the same URL and options can reuse a result within a single render of the React component tree. Route Handlers are outside that tree and therefore outside React request memoization. [Next.js 15 request memoization guide](https://nextjs.org/docs/15/app/guides/caching#request-memoization)

Other async tasks that can benefit from memoization include the following.

- Database queries (Prisma, Drizzle, etc.)
- Heavy computations
- Authentication checks
- File system operations
- Any non-fetch async work

Use `React.cache()` to deduplicate these operations across your component tree.

Reference: [React.cache documentation](https://react.dev/reference/react/cache)
