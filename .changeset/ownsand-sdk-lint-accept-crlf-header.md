---
"ownsand-sdk": patch
---

The SDK source lint now accepts an Apache-2.0 SPDX header line that ends in CRLF as well as LF, so a checkout with `core.autocrlf=true` passes `pnpm lint`. A missing, different or unterminated header still fails with "missing the Apache-2.0 SPDX header". `scripts/lint.mjs` also accepts an optional source root as its first argument; without it, it lints `src/` as before.
