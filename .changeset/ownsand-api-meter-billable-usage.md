---
"ownsand-api": minor
---

Meter billable usage and report it to `delino-accounts`: relay egress, exported-snapshot R2 storage, and Class A and Class B RPCs are counted per organization and 5-minute window, sealed into a Postgres usage outbox with their micro-USD amounts, and reported with `ReportUsage` through a Logto machine-to-machine token, with retries until `accounts-api` accepts them. New settings `ACCOUNTS_API_ORIGIN`, `LOGTO_TOKEN_ENDPOINT`, `ACCOUNTS_M2M_CLIENT_ID`, `ACCOUNTS_M2M_CLIENT_SECRET` and `METRICS_ADDR` (internal Prometheus listener with the report lag and outbox depth); without the machine-to-machine credential, usage stays in the outbox and the reporter is idle.
