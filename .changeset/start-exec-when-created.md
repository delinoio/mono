---
"ownsand-sdk": patch
---

`exec` now sends its first `ExecRequest` before it returns the handle, and no longer when the caller first iterates the handle or calls `wait()`. A caller that waits more than 15 minutes before the first read no longer gets a `not_found` rejection for an expired operation ID. The handle keeps its `execId` and a retry is still deduplicated. Nobody holds the first event before the first read, so a handle that is never read lets the 90-second idle limit cancel its stream, and the first `wait()` or iteration after that continues with `AttachExec` without a second `ExecRequest`. `attachExec` is unchanged and still sends nothing before it is read.
