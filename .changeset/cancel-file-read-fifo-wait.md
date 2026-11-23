---
"ownsand-cli": patch
---

`ownsand file read --to <FIFO>` now ends when the command is interrupted (Ctrl-C) while it waits for a reader to open the FIFO, before it sends any request. Before, the blocked open ignored the cancellation and the command kept waiting. A FIFO with an attached reader still receives the contents.
