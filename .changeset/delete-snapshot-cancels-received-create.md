---
"ownsand-agent": patch
---

A `delete_snapshot` command that arrives after a `create_snapshot` command of the same snapshot ID, and before that create ended, now cancels the create on the same channel. The agent registers each `create_snapshot` in the order in which it receives the commands, so the delete is no longer lost when it runs before the create reaches the snapshot code or while the create waits for its sandbox's command lock. The create fails with `ABORTED` and captures nothing, or discards its capture, so the device keeps no local copy and reports no snapshot until a reconnect. Before, such a delete succeeded without effect and the create then published a ready copy of a snapshot that the control plane had deleted.
