---
"ownsand-agent": patch
---

The Windows guest helper can read files at long drive and UNC paths that end in a period or an ASCII space. The snapshot checks and the file open now resolve these paths using the same Windows rules. Use a Windows guest image built with the updated helper to get the fix.
