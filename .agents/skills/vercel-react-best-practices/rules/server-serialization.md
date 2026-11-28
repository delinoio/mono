---
title: Minimize Serialization at RSC Boundaries
impact: HIGH
impactDescription: reduces data transfer size
tags: server, rsc, serialization, props
---

## Minimize Serialization at RSC Boundaries

Props passed from a Server Component to a Client Component must use values that React can serialize. React encodes those props in the React Server Component (RSC) payload. When React reads the payload, numbers and booleans retain their types, as do supported values such as arrays and plain objects with serializable contents. See [React's supported serializable types](https://react.dev/reference/rsc/use-client#serializable-types).

On the first App Router load, the browser displays the server-rendered HTML as a preview. React uses the RSC payload to reconcile the Server and Client Component trees. On later client-side navigations, Next.js uses the RSC payload, which it can prefetch and cache, and renders Client Components in the browser. See [Next.js first-load behavior](https://nextjs.org/docs/app/getting-started/server-and-client-components#on-the-client-first-load) and [subsequent navigations](https://nextjs.org/docs/app/getting-started/server-and-client-components#subsequent-navigations).

To keep the RSC payload small, pass only the fields that the Client Component needs.

**Incorrect (serializes all 50 fields):**

```tsx
async function Page() {
  const user = await fetchUser()  // 50 fields
  return <Profile user={user} />
}

'use client'
function Profile({ user }: { user: User }) {
  return <div>{user.name}</div>  // uses 1 field
}
```

**Correct (serializes only 1 field):**

```tsx
async function Page() {
  const user = await fetchUser()
  return <Profile name={user.name} />
}

'use client'
function Profile({ name }: { name: string }) {
  return <div>{name}</div>
}
```
