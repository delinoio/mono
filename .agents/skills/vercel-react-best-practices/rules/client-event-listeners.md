---
title: Deduplicate Global Event Listeners
impact: LOW
impactDescription: single listener for N components
tags: client, swr, event-listeners, subscription
---

## Deduplicate Global Event Listeners

Share one window listener across all instances of the hook. Keep the handler and callback registry in the same module so instances under different SWR cache providers share the listener. Attach the listener when the first callback is registered and remove it when the last registration is removed.

**Incorrect (N instances = N listeners):**

```tsx
function useKeyboardShortcut(key: string, callback: () => void) {
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (e.metaKey && e.key === key) {
        callback()
      }
    }
    window.addEventListener('keydown', handler)
    return () => window.removeEventListener('keydown', handler)
  }, [key, callback])
}
```

When using the `useKeyboardShortcut` hook multiple times, each instance will register a new listener.

**Correct (N instances = 1 listener):**

```tsx
import { useEffect } from 'react'

const keyCallbacks = new Map<string, Set<() => void>>()

function handleKeydown(event: KeyboardEvent) {
  if (event.metaKey) {
    keyCallbacks.get(event.key)?.forEach(callback => callback())
  }
}

function useKeyboardShortcut(key: string, callback: () => void) {
  useEffect(() => {
    // Wrap the callback so consumers that share a function get separate registrations.
    const registration = () => callback()
    const isFirstRegistration = keyCallbacks.size === 0
    const callbacks = keyCallbacks.get(key) ?? new Set<() => void>()

    callbacks.add(registration)
    keyCallbacks.set(key, callbacks)
    if (isFirstRegistration) {
      window.addEventListener('keydown', handleKeydown)
    }

    return () => {
      callbacks.delete(registration)
      if (callbacks.size === 0) {
        keyCallbacks.delete(key)
      }
      if (keyCallbacks.size === 0) {
        window.removeEventListener('keydown', handleKeydown)
      }
    }
  }, [key, callback])
}

function Profile() {
  // Multiple shortcuts will share the same listener
  useKeyboardShortcut('p', () => { /* ... */ }) 
  useKeyboardShortcut('k', () => { /* ... */ })
  // ...
}
```
