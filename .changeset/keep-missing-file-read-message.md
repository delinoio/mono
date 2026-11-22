---
"ownsand-sdk": patch
---

A file read of a path that does not exist now fails with a `DataPathError` with the code `not_found` and the guest's message (for example `read: file does not exist`), with the original Connect error as `cause`, on the relay and on peer-to-peer. Only a read whose `read_id` expired keeps the message `the file read expired`.
