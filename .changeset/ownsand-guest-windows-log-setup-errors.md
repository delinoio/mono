---
"ownsand-agent": patch
---

The Windows guest helper reports failures to create its state directory or open `guest.log` in the Windows Application Event Log. The diagnostic identifies the failed operation and includes the Windows error code and system message when available. The fix applies to guest images that include the updated helper.
