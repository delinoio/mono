---
"ownsand-cli": patch
---

Cancel `file read --to` on the first Ctrl-C when a full FIFO blocks the download on Linux or macOS. The reader can still drain the bytes already in the FIFO.
