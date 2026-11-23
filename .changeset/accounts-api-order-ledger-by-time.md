---
"accounts-api": patch
---

Order ledger entries by creation time and then by descending ID in `ListLedgerEntries`, and add the matching index with migration `00008_ledger_entries_by_time.sql`. A page token that carries only an ID resumes the previous traversal order.
