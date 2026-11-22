---
"ownsand-api": patch
---

Remove the local copy of an imported snapshot when the snapshot record was deleted before the import result arrived. The server now records the copy under a lock on the snapshot row, and a result for a deleted snapshot makes it send `delete_snapshot` to the reporting device instead of failing on a missing parent row.
