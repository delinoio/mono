---
"ownsand-sdk": patch
---

If the caller cancels after an exec stream reports an output gap, the next read and `wait()` reject with the caller's abort reason before the SDK delivers the retained output.
