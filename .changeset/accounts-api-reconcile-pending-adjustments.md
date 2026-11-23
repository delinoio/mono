---
"accounts-api": patch
---

Apply the pending refunds and chargebacks of a purchase whose credit grant already exists. A grant that the previous release created does not drain `pending_credit_adjustments`, so those rows stayed unapplied. The new `reconcile_pending_adjustments` worker task, which also runs at startup, and a payment event for an existing grant now apply and delete them once through `credit_refunds`.
