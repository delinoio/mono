---
"ownsand-agent": patch
---

The guest helper now keeps retrying the removal of an expired read snapshot that it failed to remove. Before, a failed removal was logged once and the snapshot file stayed on the guest disk until the next helper start. Now the helper keeps the snapshot as a pending removal, independent of its 30-minute memory of expired `read_id` values, and retries it in every 30-second sweep until it succeeds or the file is already gone. The `read_id` still fails with `read expired` in the meantime, and the helper logs one path-free warning for the failure, not one per retry. A new read with the same `read_id` after that memory ended first retries the pending removal and fails with `unavailable` while it still fails, so the retry cannot delete the new snapshot.
