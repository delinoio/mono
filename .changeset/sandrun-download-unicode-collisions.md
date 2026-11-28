---
"sandrun-cli": patch
---

Reject directory downloads when canonically equivalent archive paths resolve to the same local entry. The error shows both spellings, and the existing destination stays intact.
