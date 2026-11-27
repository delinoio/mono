---
"ownsand-agent": patch
---

Allow Windows guest exec to use a bare program name when an absolute request `PATH` entry selects the same executable as the helper's current directory. Skip empty entries in the request `PATH`.
