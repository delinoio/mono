---
"ownsand-agent": patch
---

`ownsand-agent status` now reports a sandbox record inventory or a snapshot directory that it cannot read: the affected count shows `unavailable` instead of a partial or zero count, a diagnostic on standard error names the inventory, the other inventory still prints and the command exits with code 1. Successful reads keep their counts and exit code 0.
