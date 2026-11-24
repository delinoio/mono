---
"ownsand-agent": patch
---

Hyper-V sandbox creation now stops when a delete or TTL expiry cancels disk preparation for a Linux or Windows guest. The agent cancels disk copies, waits for any resize in progress to return, and removes the sandbox directory before starting a VM. A control channel disconnect still allows creation to finish.
