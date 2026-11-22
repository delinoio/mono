---
"ownsand-agent": patch
---

An exec whose first connection to the guest helper fails before any request is sent now sends its start request again on the next connection and runs the command once. Before, the agent attached to an exec that the helper had never seen and ended the exec with the failure "exec is unknown or its result is no longer kept". After any other failure of the first request (an error after a successful send, a lost response, a restarted helper), the agent still only attaches, so a command that may have run never starts again.
