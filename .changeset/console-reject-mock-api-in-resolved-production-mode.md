---
"ownsand-console": patch
---

Reject `PUBLIC_OWNSAND_MOCK_API=true` before compilation in every build whose resolved Rsbuild mode is production, also when the build inherits another `NODE_ENV`, so a production bundle no longer fails in the browser.
