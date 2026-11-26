---
title: Early Return from Functions
impact: LOW-MEDIUM
impactDescription: avoids unnecessary computation
tags: javascript, functions, optimization, early-return
---

## Early Return from Functions

Use an early return to skip work once you know the result.

**Full scan**

Each failure replaces the previous error. The full scan returns the last error it finds.

```typescript
function validateUsers(users: User[]) {
  let hasError = false
  let errorMessage = ''
  
  for (const user of users) {
    if (!user.email) {
      hasError = true
      errorMessage = 'Email required'
    }
    if (!user.name) {
      hasError = true
      errorMessage = 'Name required'
    }
  }
  
  return hasError ? { valid: false, error: errorMessage } : { valid: true }
}
```

**Early return**

Start with the last user and check name before email. The first failure in this order is the one the full scan would return.

```typescript
function validateUsers(users: User[]) {
  for (let i = users.length - 1; i >= 0; i--) {
    const user = users[i]
    if (!user.name) {
      return { valid: false, error: 'Name required' }
    }
    if (!user.email) {
      return { valid: false, error: 'Email required' }
    }
  }

  return { valid: true }
}
```
