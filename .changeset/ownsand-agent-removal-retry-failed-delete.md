---
"ownsand-agent": patch
---

A device removal now keeps a sandbox whose runtime deletion failed. Before, the clean-up forgot every sandbox and wiped the sandbox records and VM files even when the runtime failed to delete a VM, so the retry found nothing to delete and could leave that VM running. Now the clean-up wipes the local data only after every runtime deletion succeeded, and the retry deletes the failed sandbox again before the agent confirms the removal.
