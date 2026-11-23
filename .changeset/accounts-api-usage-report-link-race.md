---
"accounts-api": patch
---

Serialize the first link of an organization with its unattributed usage reports through a per-organization advisory lock, so a report that finds a link that committed before the report's last attribution read is charged to that link instead of being stored as unattributed. A report that commits as unattributed first keeps that result even when a first link that waited for it starts before the end of the report window.
