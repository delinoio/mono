---
"delino-docs": patch
"delino-site": patch
---

Make the brand contrast e2e checks fail when a checked Rspress custom property is undefined, and pin the dark `--rp-c-brand-light` and `--rp-c-brand-lighter` values. An undefined variable no longer passes the contrast check through the inherited text color.
