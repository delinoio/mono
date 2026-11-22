---
"ownsand-agent": patch
---

The guest helper now reports a unix socket bind that fails with Winsock error 10048 (`WSAEADDRINUSE`) on Windows as an address-in-use error, the same as `EADDRINUSE` on other platforms. Before, the Windows build compared the error only with the placeholder `EADDRINUSE` value of the Go `syscall` package, which a Winsock error never carries, so the failure appeared as a generic bind failure.
