---
"ownsand-agent": patch
---

The OwnSand guest helper now retries removal of partial read snapshots left by failed preparation. Once cleanup succeeds, callers can retry with the same read ID. This fix requires a guest image that contains the updated guest helper.
