---
"accounts-api": patch
---

Return a representable result when a monthly usage sum exceeds the `int64` range: `GetOrganizationBilling` and `ListUsageSummary` now saturate the quantity and amount sums, and the summary total, at 9223372036854775807 instead of failing with an internal error. Stored usage reports and `ReportUsage` are unchanged.
