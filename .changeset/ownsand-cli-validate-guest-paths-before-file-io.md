---
"ownsand-cli": patch
---

`ownsand file read` and `ownsand file write` now check the guest path before opening local files. A relative guest path exits with status 2 immediately, even if the local path is a named pipe with no peer.
