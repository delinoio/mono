---
"accounts-api": patch
---

The worker that claims a queued automatic top-up now checks the current auto top-up settings before it checks the subscription. A queued top-up that the settings no longer allow (automatic top-up is off, the amount is above the current amount, or the monthly cap has no room) closes as `settings_changed` even when the subscription is canceled. It no longer counts as a failed automatic top-up and no longer sends the payment failure email or turns automatic top-up off. A top-up that the settings still allow but that has no usable subscription still fails as `no_subscription` and counts as a failure.
