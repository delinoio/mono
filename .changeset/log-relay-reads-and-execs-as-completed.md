---
"ownsand-sdk": patch
---

The logger of the SDK now records a relay `ReadFile`, `Exec` or `AttachExec` call that ran to its terminal event (the file `end` event, or `exited`, `failed` or `sandboxEnded`) as one `rpc` record with the code `ok`. Before, such a call was recorded as `Canceled`, because the SDK closed the stream right after the terminal event. A caller abort or a consumer that stops reading before the terminal event is still recorded as `Canceled`, and a server error is still recorded with its Connect code.
