---
title: Cache Storage API Calls
impact: LOW-MEDIUM
impactDescription: reduces expensive I/O
tags: javascript, localStorage, storage, caching, performance
---

## Cache Storage API Calls

`localStorage`, `sessionStorage`, and `document.cookie` are synchronous and expensive. Cache reads in memory.

**Incorrect (reads storage on every call):**

```typescript
function getTheme() {
  return localStorage.getItem('theme') ?? 'light'
}
// Called 10 times = 10 storage reads
```

**Correct (Map cache):**

```typescript
const storageCache = new Map<string, string | null>()

function getLocalStorage(key: string) {
  if (!storageCache.has(key)) {
    storageCache.set(key, localStorage.getItem(key))
  }
  return storageCache.get(key)
}

function setLocalStorage(key: string, value: string) {
  localStorage.setItem(key, value)
  storageCache.set(key, value)  // keep cache in sync
}
```

Use a Map (not a hook) so it works everywhere: utilities, event handlers, not just React components.

**Cookie caching:**

```typescript
let cookieCache: Map<string, string> | null = null

function parseCookies(cookieString: string) {
  const cookies = new Map<string, string>()
  if (cookieString === '') return cookies
  for (const pair of cookieString.split('; ')) {
    // Split at the first '=' only: a value may contain '=' (for example base64 padding)
    const separator = pair.indexOf('=')
    if (separator === -1) cookies.set('', pair)  // a cookie sent without a name
    else cookies.set(pair.slice(0, separator), pair.slice(separator + 1))
  }
  return cookies
}

function getCookie(name: string) {
  cookieCache ??= parseCookies(document.cookie)
  return cookieCache.get(name)
}

// Call after your own code writes document.cookie, so the next read sees the change
export function invalidateCookieCache() {
  cookieCache = null
}
```

Values stay raw: the cache does not decode them.

**Important (invalidate on external changes):**

If storage can change externally (another tab, server-set cookies), invalidate cache.
Cookie changes fire no `storage` event, so the cookie cache can only be reset on a known mutation (`invalidateCookieCache()`) or when the page becomes visible again:

```typescript
window.addEventListener('storage', (e) => {
  if (e.storageArea !== localStorage) return
  // key is null when clear() ran in another tab; '' is a valid key
  if (e.key === null) storageCache.clear()
  else storageCache.delete(e.key)
})

document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible') {
    storageCache.clear()
    invalidateCookieCache()  // the next getCookie() call parses document.cookie again
  }
})
```
