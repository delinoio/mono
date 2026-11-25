---
"ownsand-agent": patch
---

Fix process runtime snapshot capture and restore when the agent umask masks owner write or search permission. New copy directories get temporary owner access so the agent can fill them or remove a partial copy. Completed copies preserve the source permissions.
