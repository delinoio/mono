---
"ownsand-api": patch
---

Return the canceled or deadline-exceeded result, not OK, when a relay stream reaches its rotation after the caller canceled the request or its deadline passed. The call meter no longer counts such a stream as a completed Class A call, and the relay log records the real code. A rotation with a live request still ends with OK and no final event.
