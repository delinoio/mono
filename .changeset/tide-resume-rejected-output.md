---
"tide-cli": patch
---

Preserve the resume offset when the Tide Go SDK output callback rejects a stdout or stderr chunk, so an attach can replay the retained bytes.
