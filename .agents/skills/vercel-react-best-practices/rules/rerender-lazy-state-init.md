---
title: Use Lazy State Initialization
impact: MEDIUM
impactDescription: wasted computation on every render
tags: react, hooks, useState, performance, initialization
---

## Use Lazy State Initialization

Pass a function to `useState` for expensive initial values. Without the function form, the initializer runs on every render even though the value is only used once.

**Incorrect (runs on every render):**

```tsx
function FilteredList({ items }: { items: Item[] }) {
  // buildSearchIndex() runs on EVERY render, even after initialization
  const [searchIndex, setSearchIndex] = useState(buildSearchIndex(items))
  const [query, setQuery] = useState('')
  
  // When query changes, buildSearchIndex runs again unnecessarily
  return <SearchResults index={searchIndex} query={query} />
}

function UserProfile() {
  // Browser-only: reads localStorage. JSON.parse runs on every render
  const [settings, setSettings] = useState(
    JSON.parse(localStorage.getItem('settings') || '{}')
  )
  
  return <SettingsForm settings={settings} onChange={setSettings} />
}
```

**Correct (runs only once):**

```tsx
function FilteredList({ items }: { items: Item[] }) {
  // buildSearchIndex() runs ONLY on initial render
  const [searchIndex, setSearchIndex] = useState(() => buildSearchIndex(items))
  const [query, setQuery] = useState('')
  
  return <SearchResults index={searchIndex} query={query} />
}

// Browser-only: reads localStorage during the initial render. Use it in a
// client-only component or a client-rendered app (for example a Vite SPA),
// never in a component that is also server-rendered.
function UserProfile() {
  // JSON.parse runs only on initial render
  const [settings, setSettings] = useState(() => {
    const stored = localStorage.getItem('settings')
    return stored ? JSON.parse(stored) : {}
  })
  
  return <SettingsForm settings={settings} onChange={setSettings} />
}
```

The lazy initializer runs during the component's initial render. When the component is server-rendered, that render happens on the server, where `localStorage` does not exist, so the initializer throws. Even where `localStorage` exists, reading it during the first render can make the first client render differ from the server HTML.

**Correct (server-rendered: deterministic first render, saved settings loaded after hydration):**

```tsx
const defaultSettings: Settings = {}

function UserProfile() {
  // Same defaults on the server and on the first client render
  const [settings, setSettings] = useState<Settings>(defaultSettings)

  useEffect(() => {
    // Effects run only in the browser, after hydration
    try {
      const stored = localStorage.getItem('settings')
      if (stored) setSettings(JSON.parse(stored))
    } catch {}
  }, [])

  return <SettingsForm settings={settings} onChange={setSettings} />
}
```

When no `settings` key is stored, the defaults stay. The pure `buildSearchIndex` lazy initializer above is safe to server-render because it reads no browser API.

Use lazy initialization when computing initial values from localStorage/sessionStorage or the DOM (in browser-only components; see the server-rendered variant above), building data structures (indexes, maps), or performing heavy transformations.

For simple primitives (`useState(0)`), direct references (`useState(props.value)`), or cheap literals (`useState({})`), the function form is unnecessary.
