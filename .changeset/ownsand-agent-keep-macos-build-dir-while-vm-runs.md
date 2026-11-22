---
"ownsand-agent": patch
---

The macOS image build now stops a failed first-boot VM within a 10 second bound and reports the stop failure together with the original error. Before, the cleanup waited without a limit for the stop notification and dropped the stop error. When the VM may still run, the build keeps the staged build directory instead of deleting a disk that the VM may still use.
