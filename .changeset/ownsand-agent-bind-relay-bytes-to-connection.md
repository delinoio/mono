---
"ownsand-agent": patch
---

The relay now counts the received bytes of a response for the connection that issued the request. Before, a read of a retired response that completed after the replacement connection was installed refreshed the replacement's receive clock, which delayed its receive timeout and echo probe. Partial frames of the active response still refresh its own clock, and the timeout thresholds and bounded teardown are unchanged.
