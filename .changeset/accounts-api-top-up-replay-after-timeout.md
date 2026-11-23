---
"accounts-api": patch
---

A repeated manual top-up key within 24 hours now returns success after the 1-hour timeout when Paddle had accepted the charge, instead of `failed_precondition`. Migration `00007_top_up_provider_accept_time.sql` adds a nullable `top_ups.provider_accept_time`, set when Paddle accepts the charge; apply it before the new binary runs. A timed-out top-up without that evidence (an unknown result or a row from before the migration) returns `unavailable`, and a rejected or payment-failed top-up still returns `failed_precondition`.
