---
"ownsand-agent": patch
---

Stop the restore attempt when a sandbox create is canceled during memory restore on a macOS host. The agent returns the create context error and skips disk fallback for both Linux and macOS guests.
