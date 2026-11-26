---
"ownsand-console": patch
---

Honor fractional enrollment token TTLs in the mock API. Store the exact expiry for valid TTLs up to 24 hours and reject invalid TTLs before creating a token.
