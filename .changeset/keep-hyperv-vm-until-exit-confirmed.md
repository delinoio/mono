---
"ownsand-agent": patch
---

On Windows hosts, deleting a Hyper-V sandbox now removes its VM directory and registration only after the exit of the compute system is confirmed. A compute system that opens but whose exit event does not arrive within the stop timeout, or whose terminate call fails, is an unknown outcome: the delete returns an error and keeps the files, so the sandbox manager retries it. A failed create with an unconfirmed stop keeps the registration and directory in the same way. A system that is absent or stopped already still lets the delete finish.
