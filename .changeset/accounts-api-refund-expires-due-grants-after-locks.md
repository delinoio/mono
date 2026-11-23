---
"accounts-api": patch
---

A refund or chargeback webhook for a purchase now reads the clock again after it holds the purchase lock and the billing account lock. Before, it used the time from before the lock wait, so a credit grant that expired during the wait could still be consumed by the refund. Now the grant is expired first (an `EXPIRY` ledger entry before the `REFUND` entry), and the part of the refund that no usable grant covers becomes debt. The refund cap and the one-time application of each adjustment ID do not change.
