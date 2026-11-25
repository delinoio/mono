---
"ownsand-cli": patch
"tide-cli": patch
---

Keep credential locks usable under restrictive umasks on Linux and macOS. The CLIs restore an existing lock's owner permissions in place so login, token refresh, and logout can use it again.
