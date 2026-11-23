---
"ownsand-agent": patch
---

The guest helper no longer hangs when a file that a read snapshot is preparing is replaced by a FIFO between the check and the open on Linux and macOS guests. Before, the open of the FIFO blocked forever, so the snapshot never became ready and the helper never reclaimed its entry. The open now returns at once, and the read fails with `aborted` so that a retry of the `read_id` starts a new snapshot.
