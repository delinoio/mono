---
"ownsand-agent": patch
---

Agent shutdown now waits for every exec that it already admitted and refuses an exec that arrives after shutdown started with `unavailable`. Before, `Close` could return while a request handler had admitted an exec but not yet registered its forwarding worker, so that worker started after shutdown and was not waited for.
