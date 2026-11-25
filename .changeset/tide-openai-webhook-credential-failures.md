---
"tide-api": patch
---

Return HTTP 500 when Tide cannot read or decrypt a stored OpenAI webhook secret so OpenAI can retry the delivery. Keep HTTP 401 for missing secrets and invalid signatures.
