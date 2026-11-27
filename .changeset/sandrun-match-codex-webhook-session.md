---
"sandrun-api": patch
---

Verify that the session returned by OpenAI matches the signed Codex webhook before queuing work or releasing a runner. A missing or different session ID leaves the delivery eligible for retry.
