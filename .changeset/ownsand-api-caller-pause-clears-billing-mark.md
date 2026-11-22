---
"ownsand-api": patch
---

`PauseSandbox` on a sandbox that billing already paused now removes the billing pause mark before it answers `PAUSED`, so a confirmed billing recovery no longer resumes a sandbox that a caller paused. Before, the mark was removed only when the call sent a pause command. The caller's pause and the billing worker's pause or resume of the same sandbox now run one at a time under a Postgres advisory lock that every API instance shares: a recovery that selected the sandbox before the caller paused it sends no resume, and a pause that starts while a resume is in flight waits for the agent's result and then pauses the sandbox if it resumed. A failure to remove the mark now fails the call with `internal`.
