---
"ownsand-agent": patch
---

A `delete_snapshot` whose removal of the snapshot directory fails no longer drops the snapshot from the agent state. The entry stays in the state report and stays unusable until a removal succeeds, the immediate delete reports `INTERNAL`, a removal after the last reader's release is logged and retried by the control plane's next delete, and a repeated delete retries the removal. The agent serializes removals per snapshot ID, so a late cleanup never deletes the content of a replacement copy, and an import first finishes the removal of an old copy of the same ID.
