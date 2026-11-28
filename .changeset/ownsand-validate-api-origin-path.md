---
"ownsand-cli": patch
---

Reject OwnSand API origins with repeated trailing slashes. If the CLI reports an invalid origin, remove the extra slashes from `--api-url` or `OWNSAND_API_URL`.
