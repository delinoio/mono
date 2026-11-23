---
"ownsand-cli": patch
---

A failed write of the guest output to standard output in a text-mode `ownsand sandbox exec` (for example a full disk or `/dev/full`) now exits with status `1` and prints the write error on standard error. Before, it exited with `255` and a `data path:` prefix, as if the data path had failed. A real data path failure still exits with `255`.
