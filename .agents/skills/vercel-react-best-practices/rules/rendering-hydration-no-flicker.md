---
title: Prevent Hydration Mismatch Without Flickering
impact: MEDIUM
impactDescription: avoids visual flicker and hydration errors
tags: rendering, ssr, hydration, localStorage, flicker
---

## Prevent Hydration Mismatch Without Flickering

When rendering content that depends on client-side storage (localStorage, cookies), avoid both SSR breakage and post-hydration flickering by injecting a synchronous script in the document head that sets the value before any body markup can paint and before React hydrates.

**Incorrect (breaks SSR):**

```tsx
function ThemeWrapper({ children }: { children: ReactNode }) {
  // localStorage is not available on server - throws error
  const theme = localStorage.getItem('theme') || 'light'
  
  return (
    <div className={theme}>
      {children}
    </div>
  )
}
```

Server-side rendering will fail because `localStorage` is undefined.

**Incorrect (visual flickering):**

```tsx
function ThemeWrapper({ children }: { children: ReactNode }) {
  const [theme, setTheme] = useState('light')
  
  useEffect(() => {
    // Runs after hydration - causes visible flash
    const stored = localStorage.getItem('theme')
    if (stored) {
      setTheme(stored)
    }
  }, [])
  
  return (
    <div className={theme}>
      {children}
    </div>
  )
}
```

Component first renders with default value (`light`), then updates after hydration, causing a visible flash of incorrect content.

**Correct (theme set in the head before body markup streams, expected mismatch suppressed):**

```tsx
function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en" suppressHydrationWarning>
      <head>
        {/* Classic inline script: no async, defer or type="module" */}
        <script
          dangerouslySetInnerHTML={{
            __html: `
              (function() {
                try {
                  var theme = localStorage.getItem('theme') || 'light';
                  document.documentElement.setAttribute('data-theme', theme);
                } catch (e) {}
              })();
            `,
          }}
        />
        <style>{`
          body { background: #fff; color: #111; }
          html[data-theme='dark'] body { background: #111; color: #eee; }
        `}</style>
      </head>
      <body>{children}</body>
    </html>
  )
}
```

The bootstrap is a classic inline script in the document head, before the `<body>`. A classic script blocks the HTML parser, so it runs before any theme-dependent body markup is streamed or painted, and the first paint already uses the stored theme (or the `light` fallback when nothing is stored or storage throws). Do not use `async`, `defer` or `type="module"`, which run later, and do not place the script after the themed markup: the browser can paint the default theme first. The script sets one attribute on `document.documentElement`, so the server-rendered `<html>` differs from the DOM by that attribute. Put `suppressHydrationWarning` only on the `<html>` element that React owns and whose attribute the script changes; it covers only that element's own attributes, so do not add it to the body or other children. React keeps the attribute the script set during hydration, so the bootstrap theme survives without a mismatch warning.

This pattern is especially useful for theme toggles, user preferences, authentication states, and any client-only data that should render immediately without flashing default values.
