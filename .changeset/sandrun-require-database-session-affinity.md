---
"sandrun-api": patch
---

Reject PlanetScale transaction-pooled database endpoints at startup. Use the direct primary endpoint to keep the PostgreSQL session that holds the organization deletion lock.
