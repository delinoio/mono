---
"ownsand-cli": patch
---

`ownsand help`, `ownsand --help` and `ownsand <command> <subcommand> --help` now exit with status 1 and the write error on standard error when standard output rejects the help (a full disk, a closed pipe or `/dev/full`). Before, they exited with 0 and dropped the error.
