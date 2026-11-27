---
"ownsand-agent": patch
---

Validate the Windows ISO flag and guest helper before opening agent state. The command exits with code 2 for a missing Windows ISO flag or an invalid explicit guest executable. Rejected inputs leave agent state untouched.
