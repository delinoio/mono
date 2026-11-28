---
title: Cache Repeated Function Calls
impact: MEDIUM
impactDescription: avoid redundant computation
tags: javascript, cache, memoization, performance
---

## Cache Repeated Function Calls

Use a module-level Map to cache function results when the same function is called repeatedly with the same inputs during render.

**Incorrect (redundant computation):**

```typescript
function ProjectList({ projects }: { projects: Project[] }) {
  return (
    <div>
      {projects.map(project => {
        // slugify() called 100+ times for same project names
        const slug = slugify(project.name)
        
        return <ProjectCard key={project.id} slug={slug} />
      })}
    </div>
  )
}
```

**Correct (cached results):**

```typescript
// Module-level cache
const slugifyCache = new Map<string, string>()

function cachedSlugify(text: string): string {
  if (slugifyCache.has(text)) {
    return slugifyCache.get(text)!
  }
  const result = slugify(text)
  slugifyCache.set(text, result)
  return result
}

function ProjectList({ projects }: { projects: Project[] }) {
  return (
    <div>
      {projects.map(project => {
        // Computed only once per unique project name
        const slug = cachedSlugify(project.name)
        
        return <ProjectCard key={project.id} slug={slug} />
      })}
    </div>
  )
}
```

**Simpler pattern for a single value**

```typescript
let hasThemeCookieCache: boolean | null = null

function hasThemeCookie(): boolean {
  if (hasThemeCookieCache !== null) {
    return hasThemeCookieCache
  }

  hasThemeCookieCache = document.cookie
    .split(';')
    .some(entry => entry.trimStart().startsWith('theme='))
  return hasThemeCookieCache
}

function invalidateThemeCookieCache() {
  hasThemeCookieCache = null
}
```

This example checks for the `theme` preference cookie by its exact, case-sensitive name.
An empty value counts as present. Call `invalidateThemeCookieCache()` after the cookie changes.
The next call to `hasThemeCookie()` reads the current cookie string.

Use a Map (not a hook) so it works everywhere: utilities, event handlers, not just React components.

Reference: [How we made the Vercel Dashboard twice as fast](https://vercel.com/blog/how-we-made-the-vercel-dashboard-twice-as-fast)
