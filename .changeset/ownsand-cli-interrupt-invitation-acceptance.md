---
"ownsand-cli": patch
---

When Ctrl-C cancels an in-progress `ownsand member accept` request, the command prints `error: interrupted` and exits with status 130.
