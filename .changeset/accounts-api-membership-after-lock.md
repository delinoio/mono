---
"accounts-api": patch
---

Fix billing account mutations that used the caller's role from before a concurrent membership change: a caller that waited for the billing account lock now gets `not_found` after removal and `permission_denied` after a demotion or an ownership transfer instead of the mutation succeeding.
