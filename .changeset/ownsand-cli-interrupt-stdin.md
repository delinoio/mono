---
"ownsand-cli": patch
---

Make `member accept` and `file write` end with status `130` when Ctrl-C interrupts them while they wait for data on standard input, and let a second Ctrl-C end any command that is still running.
