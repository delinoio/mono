---
title: Preload Based on User Intent
impact: MEDIUM
impactDescription: reduces perceived latency
tags: bundle, preload, user-intent, hover
---

## Preload Based on User Intent

Preload heavy bundles before they're needed to reduce perceived latency.

**Example (preload on hover/focus):**

```tsx
function EditorButton({ onClick }: { onClick: () => void }) {
  const preload = () => {
    if (typeof window !== 'undefined') {
      // Best effort: handle a failed chunk load here so it is not an
      // unhandled rejection. Opening the editor still loads it on demand
      void import('./monaco-editor').catch(error => {
        console.warn('Editor preload failed', error)
      })
    }
  }

  return (
    <button
      onMouseEnter={preload}
      onFocus={preload}
      onClick={onClick}
    >
      Open Editor
    </button>
  )
}
```

**Example (preload when feature flag is enabled):**

```tsx
function FlagsProvider({ children, flags }: Props) {
  useEffect(() => {
    if (flags.editorEnabled && typeof window !== 'undefined') {
      // The catch covers a failed import, an init() that throws and a
      // promise returned by init() that rejects
      void import('./monaco-editor')
        .then(mod => mod.init())
        .catch(error => {
          console.warn('Editor preload failed', error)
        })
    }
  }, [flags.editorEnabled])

  return <FlagsContext.Provider value={flags}>
    {children}
  </FlagsContext.Provider>
}
```

The `typeof window !== 'undefined'` check prevents bundling preloaded modules for SSR, optimizing server bundle size and build speed.

Preloading is best effort: a failed preload is only logged, and the explicit path that opens the editor (the `onClick` handler above) still loads the module on demand and reports its own errors.
