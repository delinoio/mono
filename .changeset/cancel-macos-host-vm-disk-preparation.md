---
"ownsand-agent": patch
---

Stop disk preparation when a sandbox create is canceled on a macOS host. The agent skips the remaining preparation and checks for cancellation before it starts or resumes a Linux or macOS guest.
