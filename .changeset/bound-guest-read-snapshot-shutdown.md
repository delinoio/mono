---
"ownsand-agent": patch
---

The OwnSand guest helper can now shut down within its read grace when a snapshot create or removal stalls. Once writes and write cleanup finish, a replacement helper can start and read the same ID without interference from late snapshot work. The fix requires a guest image with the updated helper.
