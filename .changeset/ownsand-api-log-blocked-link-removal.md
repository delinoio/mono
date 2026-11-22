---
"ownsand-api": patch
---

Log `billing_status_changed` and run the change hooks when an ownership transfer or an organization deletion removes the billing link of a blocked or suspended organization. Before, the removal wrote no record because the phase stayed the same.
