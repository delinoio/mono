---
"ownsand-api": patch
---

Log `startup_failure_reason=address_in_use` on the `server_exited` event when the control plane cannot bind its HTTP or STUN address because another socket holds it. The reason comes from the wrapped error number (`EADDRINUSE` on Unix, native Winsock error 10048 on Windows), and the original error stays in the event. Other startup failures are unchanged.
