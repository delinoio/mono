---
"sandrun-api": patch
---

Evict the cached OwnSand client after an organization deletion commits. Lookups that overlap eviction cannot restore the cached client.
