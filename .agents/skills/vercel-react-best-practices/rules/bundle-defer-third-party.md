---
title: Defer Non-Critical Third-Party Libraries
impact: MEDIUM
impactDescription: loads after hydration
tags: bundle, third-party, analytics, defer
---

## Defer Non-Critical Third-Party Libraries

Analytics, logging, and error tracking don't block user interaction. Load them after hydration.

**Incorrect (blocks initial bundle):**

```tsx
import { Analytics } from '@vercel/analytics/react'

export default function RootLayout({ children }) {
  return (
    <html>
      <body>
        {children}
        <Analytics />
      </body>
    </html>
  )
}
```

**Correct (loads after hydration):**

`ssr: false` is not allowed with `next/dynamic` in Server Components, so keep the dynamic import in a Client Component and render it from the Server Component layout.

```tsx
// app/analytics.tsx
'use client'

import dynamic from 'next/dynamic'

const Analytics = dynamic(
  () => import('@vercel/analytics/react').then(m => m.Analytics),
  { ssr: false }
)

export function DeferredAnalytics() {
  return <Analytics />
}
```

```tsx
// app/layout.tsx (Server Component)
import { DeferredAnalytics } from './analytics'

export default function RootLayout({ children }) {
  return (
    <html>
      <body>
        {children}
        <DeferredAnalytics />
      </body>
    </html>
  )
}
```
