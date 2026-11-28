---
"ownsand-agent": patch
---

`ownsand-agent enroll` now serializes enrollment on one state directory across processes. It holds an exclusive lock on `enrollment.lock` in the state directory for the whole sequence: the identity read, the pending key write, the `Enroll` call and the completion write. A second `enroll` waits up to 45 seconds, then reuses the pending key the first left (so a retry after a lost response still sends the same key) or stops with `agent: this device is already enrolled` and leaves the completed identity unchanged. If the command cannot acquire the lock before the wait ends, it reports `the enrollment lock is busy for this state directory` and exits with code `1`. Different state directories do not wait for each other. Before deleting data in the state directory, `reset` takes the same lock. It preserves `enrollment.lock`.
