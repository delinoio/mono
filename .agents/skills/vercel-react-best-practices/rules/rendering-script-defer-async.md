---
title: Use defer or async on Script Tags
impact: HIGH
impactDescription: eliminates render-blocking
tags: rendering, script, defer, async, performance
---

## Use defer or async on Script Tags

**Impact: HIGH (eliminates render-blocking)**

A parser-inserted external classic script blocks HTML parsing while the browser fetches and evaluates it if neither `async` nor `defer` is present. This can delay First Contentful Paint and Time to Interactive.

- With `defer` and no `async`, the browser fetches the script in parallel with parsing and evaluates it in document order after parsing finishes.
- With `async`, the browser fetches the script in parallel with parsing and evaluates it as soon as it is ready. Evaluation order can differ from document order.

Use `defer` when an external classic script needs the parsed DOM or depends on an earlier deferred script. Use `async` for independent classic scripts such as analytics.

The browser evaluates a parser-inserted module script (`type="module"`) after parsing if the `async` attribute is absent. The `defer` attribute applies only to external classic scripts. With `async`, the browser evaluates a module as soon as its module graph is ready, which can be before parsing finishes.

**Incorrect (blocks rendering):**

```tsx
export default function Document() {
  return (
    <html>
      <head>
        <script src="https://example.com/analytics.js" />
        <script src="/scripts/utils.js" />
      </head>
      <body>{/* content */}</body>
    </html>
  )
}
```

**Correct (non-blocking):**

```tsx
export default function Document() {
  return (
    <html>
      <head>
        {/* Independent script - use async */}
        <script src="https://example.com/analytics.js" async />
        {/* DOM-dependent script - use defer */}
        <script src="/scripts/utils.js" defer />
      </head>
      <body>{/* content */}</body>
    </html>
  )
}
```

**Note:** In Next.js, prefer the `next/script` component with `strategy` prop instead of raw script tags:

```tsx
import Script from 'next/script'

export default function Page() {
  return (
    <>
      <Script src="https://example.com/analytics.js" strategy="afterInteractive" />
    </>
  )
}
```

`strategy="beforeInteractive"` does not work in a page component. A site-wide script that must load before the page becomes interactive belongs in the App Router root layout or, for the Pages Router, in `pages/_document.js`.

Reference: [MDN - Script element](https://developer.mozilla.org/en-US/docs/Web/HTML/Element/script#defer)
