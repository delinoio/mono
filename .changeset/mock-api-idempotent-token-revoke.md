---
"ownsand-console": patch
---

The in-browser mock API now returns an already revoked enrollment token unchanged when it is revoked again, as the control plane does, instead of failing with `failed_precondition`. A used token still fails with `failed_precondition`.
