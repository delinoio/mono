---
"accounts-api": patch
---

Read the clock for the time of a new organization link after the link holds the billing account lock and the organization lock, so a first link starts after an unattributed usage report that committed while the link waited. The link can still start before the end of that report's window when the window ends after the server time; the stored unattributed result stands and a retry charges nothing to the link.
