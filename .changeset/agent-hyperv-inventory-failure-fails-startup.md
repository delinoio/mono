---
"ownsand-agent": patch
---

On Windows, fail the agent start when the Hyper-V runtime cannot list its compute systems. Before, a failed enumeration of the Host Compute Service (for example an access error) was logged and reported as an empty inventory, so startup reconciliation took the missing VMs for gone and could end the records of VMs that still ran, and a VM that outlived the agent process was left running. `Running` now returns the tracked VM IDs together with an error that wraps the cause, and the sandbox manager returns that error from startup reconciliation before it deletes a VM or changes a record, so the agent does not start until the inventory succeeds. A host without the Host Compute Service DLL, a successful enumeration and an empty enumeration keep their behavior.
