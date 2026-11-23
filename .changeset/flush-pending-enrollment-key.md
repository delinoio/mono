---
"ownsand-agent": patch
---

On Linux, `ownsand-agent enroll` now flushes the pending device key and its directory entries (the state directory, and the parent of each directory that was created for it) to disk before it sends the `Enroll` call. Before, a power loss after the call could lose the key while the control plane had recorded its public key for the one-time token. If the flush fails, enrollment stops before the call and the token is not used. The identity file format and permissions are unchanged. Windows and macOS behave as before.
