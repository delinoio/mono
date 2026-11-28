---
"ownsand-agent": patch
---

On Windows, the OwnSand guest helper starts with a fresh read snapshot directory when only open handles block deletion of old snapshot trees. Other cleanup failures stop startup and report the cause.
