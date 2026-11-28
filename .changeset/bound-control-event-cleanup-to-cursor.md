---
"ownsand-api": patch
---

Limit control event cleanup to the lease holder's initialized in-memory delivery cursor. Previously, every instance ran cleanup by age alone. Cleanup now runs only on the lease holder and can delete rows older than one hour whose delivery IDs are at or below the current cursor. The cursor starts at the lease baseline and advances as the holder handles later events. Historical rows at or below the baseline can age out even if the holder never replayed them. Rows above the current cursor, such as a `delete_snapshot` event still pending during a long database stall, stay even if they are older than one hour.
