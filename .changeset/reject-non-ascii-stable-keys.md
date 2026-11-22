---
"accounts-api": patch
---

Reject service and meter identifiers that only match a stable identifier after Unicode case mapping, such as `ownſand` (U+017F) in `USAGE_SERVICE_CLIENTS` or in a stored key. The configuration and the billing parsers now accept only the bytes `a-z`, `0-9` and `_` (and exactly one `.` between the two parts of a meter identifier), so such a value stops the server at startup instead of resolving to `ownsand`.
