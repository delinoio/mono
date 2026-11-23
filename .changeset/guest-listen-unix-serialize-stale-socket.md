---
"ownsand-agent": patch
---

Two guest helpers that start at the same time on one stale `unix://` socket file no longer both remove and bind it. Before, the second helper could delete the live socket file that the first helper had just bound, which left the first helper running but unreachable. Now the helpers take an advisory lock on the sidecar file `<path>.lock` around the probe, the removal and the bind, so exactly one helper binds. The other fails with the path-free in-use error and leaves the path of the winner. A helper that finds the lock held for 5 seconds fails with a path-free lock timeout error. The empty `<path>.lock` file stays next to the socket. `vsock://`, `hvsock://` and `tcp://` listeners and Linux abstract `unix://@name` addresses take no lock.
