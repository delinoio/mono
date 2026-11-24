---
"tide-api": patch
---

End a queued GitHub job request with `NO_ELIGIBLE_DEVICE` when OwnSand reports that no device can run its profile. This stops repeated sandbox creation attempts for that request.
