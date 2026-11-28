---
title: Conditional Module Loading
impact: HIGH
impactDescription: loads large data only when needed
tags: bundle, conditional-loading, lazy-loading
---

## Conditional Module Loading

Load large data or modules only when a feature is activated.

**Example (lazy-load animation frames):**

```tsx
function AnimationPlayer({ enabled, setEnabled }: { enabled: boolean; setEnabled: React.Dispatch<React.SetStateAction<boolean>> }) {
  const [frames, setFrames] = useState<Frame[] | null>(null)

  useEffect(() => {
    if (enabled && !frames && typeof window !== 'undefined') {
      import('./animation-frames.js')
        .then(mod => setFrames(mod.frames))
        .catch(() => setEnabled(false))
    }
  }, [enabled, frames, setEnabled])

  if (!frames) return <Skeleton />
  return <Canvas frames={frames} />
}
```

React runs this effect only on the client, after a render commits. In the browser, the effect evaluates the dynamic `import()` only when the feature is enabled, no frames are loaded, and `window` is available.

A server build can still emit the imported module in an async chunk. To omit the module from server assets, use a client-only boundary that your bundler excludes from the server build or a condition that the bundler can resolve to false at build time. Build for production and search the emitted server JavaScript, including async chunks, for the module or a distinctive string from it.
