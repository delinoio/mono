---
"delino-ui": patch
---

Honor explicit accessible names on Progress and Meter. Both use a nonblank `aria-labelledby` first, then a nonblank `aria-label`, then the visible label.
