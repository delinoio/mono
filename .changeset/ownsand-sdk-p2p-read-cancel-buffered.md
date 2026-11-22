---
"ownsand-sdk": patch
---

End a canceled peer-to-peer stream with the abort reason before it delivers more buffered frames, so a file read that its caller canceled while the consumer was paused yields no further chunks and a buffered final frame cannot turn the cancellation into a success. Transport failures keep delivering the frames that were queued before the failure.
