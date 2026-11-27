---
"ownsand-sdk": patch
---

`Sandbox.exec()` now starts the operation eagerly and returns the handle synchronously. The SDK sends the first `ExecRequest` asynchronously after selecting a transport and obtaining any required relay grant. The agent admits an unknown operation ID for 15 minutes after its timestamp. Dispatch proceeds independently of the first iteration or `wait()` call, so waiting to read the handle cannot cause the operation ID to expire before dispatch. Transport setup and agent admission have no timing guarantee. The handle keeps its `execId` and a retry is still deduplicated. Nobody holds the first event before the first read, so a handle that is never read lets the 90-second idle limit cancel its stream, and the first `wait()` or iteration after that continues with `AttachExec` without a second `ExecRequest`. `attachExec` is unchanged and still sends nothing before it is read.
