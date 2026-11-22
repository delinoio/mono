---
"delino-docs": patch
"delino-site": patch
---

Keep `pnpm --filter delino-docs preview` and `pnpm --filter delino-site preview` on their fixed loopback ports (13500 and 13600) when the shell exports `PORT` or `HOST`. The `preview` scripts now start `rspress preview` through `scripts/dev/rspress-preview.mjs`, which removes both variables.
