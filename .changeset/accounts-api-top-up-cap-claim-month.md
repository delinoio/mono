---
"accounts-api": patch
---

An automatic top-up now counts toward the monthly cap and `month_to_date_usd_micros` in the UTC month in which the worker claims it, not the month in which it was queued. Migration `00006_top_up_cap_time.sql` adds a nullable `top_ups.cap_time`; apply it before the new worker runs. Rows without a cap time keep counting in the month of their `create_time`.
