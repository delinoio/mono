---
"ownsand-api": patch
---

The usage reporter now skips outbox rows whose stored meter is not an OwnSand meter. Before, it sent them as an unspecified meter, `accounts-api` rejected the whole request, and the supported rows of the same batch were not reported while the retry back-off grew. The skipped rows stay in the outbox unchanged, are logged as `billing_usage_unknown_meter_unsent` and are counted by the `ownsand_billing_usage_outbox_unsupported_depth` gauge.
