---
"ownsand-cli": patch
---

Return exit status `1` and report the original write error on stderr when `ownsand sandbox exec --format json` cannot write an event to stdout.
