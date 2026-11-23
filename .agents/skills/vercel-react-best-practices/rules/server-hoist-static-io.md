---
title: Hoist Static I/O to Module Level
impact: HIGH
impactDescription: avoids repeated file/network I/O per request
tags: server, io, performance, next.js, route-handlers, og-image
---

## Hoist Static I/O to Module Level

**Impact: HIGH (avoids repeated file/network I/O per request)**

When loading static assets (fonts, logos, images, config files) in route handlers or server functions, hoist the I/O operation to module level. Cache the loaded data in module scope so it is read once per module instance, not on every request. Start asynchronous loads lazily on the first request and cache the promise: a promise created at import time has no rejection handler until a request awaits it, so a load that fails before the first request becomes an unhandled rejection. This eliminates redundant file system reads or network fetches that would otherwise run on every invocation.

**Incorrect (reads font file on every request):**

```typescript
// app/api/og/route.tsx
import { ImageResponse } from 'next/og'

export async function GET(request: Request) {
  // Runs on EVERY request - expensive!
  const fontData = await fetch(
    new URL('./fonts/Inter.ttf', import.meta.url)
  ).then(res => res.arrayBuffer())

  const logoData = await fetch(
    new URL('./images/logo.png', import.meta.url)
  ).then(res => res.arrayBuffer())

  return new ImageResponse(
    <div style={{ fontFamily: 'Inter' }}>
      {/* @ts-expect-error Satori accepts ArrayBuffer/typed arrays for <img src> at runtime */}
      <img src={logoData} />
      Hello World
    </div>,
    { fonts: [{ name: 'Inter', data: fontData }] }
  )
}
```

**Correct (loads once on the first request and caches the promise):**

```typescript
// app/api/og/route.tsx
import { ImageResponse } from 'next/og'

// Module-level cache: the first request starts the load, later and
// concurrent requests reuse the same promise. Importing this module
// starts no I/O.
let assets: Promise<{ font: ArrayBuffer; logo: ArrayBuffer }> | undefined

function loadAssets() {
  return (assets ??= Promise.all([
    fetch(new URL('./fonts/Inter.ttf', import.meta.url)).then(res =>
      res.arrayBuffer()
    ),
    fetch(new URL('./images/logo.png', import.meta.url)).then(res =>
      res.arrayBuffer()
    ),
  ]).then(([font, logo]) => ({ font, logo })))
}

export async function GET(request: Request) {
  // Awaiting right away means a failed load rejects this request
  // instead of surfacing as an unhandled rejection
  const { font, logo } = await loadAssets()

  return new ImageResponse(
    <div style={{ fontFamily: 'Inter' }}>
      {/* @ts-expect-error Satori accepts ArrayBuffer/typed arrays for <img src> at runtime */}
      <img src={logo} />
      Hello World
    </div>,
    { fonts: [{ name: 'Inter', data: font }] }
  )
}
```

**Correct (synchronous fs at module level):**

```typescript
// app/api/og/route.tsx
import { ImageResponse } from 'next/og'
import { readFileSync } from 'fs'
import { join } from 'path'

// Synchronous read at module level - blocks only during module init
const fontData = readFileSync(
  join(process.cwd(), 'public/fonts/Inter.ttf')
)

const logoData = readFileSync(
  join(process.cwd(), 'public/images/logo.png')
)

export async function GET(request: Request) {
  return new ImageResponse(
    <div style={{ fontFamily: 'Inter' }}>
      {/* @ts-expect-error Satori accepts ArrayBuffer/typed arrays for <img src> at runtime */}
      <img src={logoData} />
      Hello World
    </div>,
    { fonts: [{ name: 'Inter', data: fontData }] }
  )
}
```

**Incorrect (reads config on every call):**

```typescript
import fs from 'node:fs/promises'

export async function processRequest(data: Data) {
  const config = JSON.parse(
    await fs.readFile('./config.json', 'utf-8')
  )
  const template = await fs.readFile('./template.html', 'utf-8')

  return render(template, data, config)
}
```

**Correct (caches config and template at module level):**

```typescript
import fs from 'node:fs/promises'

function readStaticFiles() {
  return Promise.all([
    fs.readFile('./config.json', 'utf-8').then(JSON.parse),
    fs.readFile('./template.html', 'utf-8'),
  ]).then(([config, template]) => ({ config, template }))
}

// Started by the first call, shared by concurrent and later calls
let staticFiles: ReturnType<typeof readStaticFiles> | undefined

function loadStaticFiles() {
  return (staticFiles ??= readStaticFiles())
}

export async function processRequest(data: Data) {
  const { config, template } = await loadStaticFiles()

  return render(template, data, config)
}
```

The cached promise lives as long as the module instance. If a load fails (a missing file, malformed JSON, a failed fetch), the rejected promise stays cached and every later request rejects with the same error until the module is reloaded. To retry after a failure, clear the cache variable (for example `staticFiles = undefined`) in a `catch` around the awaited call.

When to use this pattern:

- Loading fonts for OG image generation
- Loading static logos, icons, or watermarks
- Reading configuration files that don't change at runtime
- Loading email templates or other static templates
- Any static asset that's the same across all requests

When not to use this pattern:

- Assets that vary per request or user
- Files that may change during runtime (use caching with TTL instead)
- Large files that would consume too much memory if kept loaded
- Sensitive data that shouldn't persist in memory

With Vercel's [Fluid Compute](https://vercel.com/docs/fluid-compute), module-level caching is especially effective because multiple concurrent requests share the same function instance. The static assets stay loaded in memory across requests without cold start penalties.

In traditional serverless, each cold start re-executes module-level code, but subsequent warm invocations reuse the loaded assets until the instance is recycled.
