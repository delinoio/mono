---
"tide-api": patch
---

Return `invalid_argument` when OwnSand rejects invalid dev session commands or file requests during SDK validation. Reject file chunks whose end offset exceeds the signed 64-bit range.
