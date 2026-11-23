---
"ownsand-api": patch
---

Keep a usage flush batch under one receipt ID so that the retry of a flush whose commit response was lost no longer counts the batch twice. Adds migration `0015_billing_usage_flush_receipts.sql` (new `usage_flush_receipts` table, additive) and a janitor job that deletes receipts after 7 days.
