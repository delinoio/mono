---
"accounts-api": patch
---

Reject organization link rows with a NULL billing status or status reason next to a status sequence or timestamp, by making the `organization_links_status_complete` check state `IS NOT NULL` for both columns.
