---
"sandrun-cli": patch
---

When a sync loses the lock command's result, SandRun checks whether the command acquired the lock. If it confirms acquisition, it releases the lock so an ordinary retry can proceed. The interrupted sync returns its original error and leaves the workspace and manifest intact.
