---
"ownsand-agent": patch
---

The guest helper no longer lets a late read snapshot worker write into the state directory of its replacement. Closing the helper now ends snapshot preparation before it releases the state directory lock: new reads fail with `unavailable`, no worker creates a snapshot file any more, and a running copy stops and removes its partial file. Snapshots live in a directory with a unique name for each helper process, so a worker that is stuck in source file system I/O, which the helper waits for at most 2 seconds, cannot create or remove anything in the replacement's snapshots.
