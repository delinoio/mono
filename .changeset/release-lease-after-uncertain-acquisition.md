---
"ownsand-api": patch
---

Delete the instance's own lease row when it drains, also when an acquisition that the database committed was never confirmed locally, so that the replacement instance acquires the lease at once instead of waiting for the TTL. The release never deletes the row of another holder.
