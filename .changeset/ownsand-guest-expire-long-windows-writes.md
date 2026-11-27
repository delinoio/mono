---
"ownsand-agent": patch
---

The OwnSand guest helper cleans up expired uploads at long Windows paths, including after the helper restarts. Cleanup preserves the upload target and any file that replaced the staging file. Use a Windows guest image built with the updated helper to get the fix.
