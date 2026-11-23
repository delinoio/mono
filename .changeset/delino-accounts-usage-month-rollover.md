---
"delino-accounts": patch
---

Keep the upper limit of the usage month picker current while the billing page stays open. When the UTC month changes, or the tab becomes visible again after a rollover whose timers were delayed, "Next month" becomes available without a reload and the selected month stays. Selecting the new current month requests that month and disables "Next month" again.
