---
"ownsand-agent": patch
---

If the agent loses its control channel while Firecracker is replying to a snapshot pause, the agent waits for the reply and resumes the sandbox before returning from the canceled command. The agent saves no snapshot. If Firecracker rejects the resume, the agent reports the sandbox as paused so an explicit resume can retry it.
