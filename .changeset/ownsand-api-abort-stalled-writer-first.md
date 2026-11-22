---
"ownsand-api": patch
---

The agent channel handler now aborts a blocked transport write right after the writer drain timeout, before the device offline update. Before, it aborted the write only after that update, so an offline update that waited for the device row lock kept the blocked write, the handler and the outbound queue until the lock was free.
