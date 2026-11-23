---
"ownsand-agent": patch
---

The Windows guest helper now reports a failed rotation of its service log. When the move of an oversized `guest.log` to `guest.log.1` fails, for example because another program holds the archive open, the helper keeps appending to the active log, leaves the earlier archive unchanged and writes one warning record to the active log; the next start rotates again.
