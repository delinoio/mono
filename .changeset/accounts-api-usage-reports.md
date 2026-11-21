---
"accounts-api": minor
---

Add the service API `UsageService` for Delino services that charge for usage: idempotent usage reports per organization, meter and 5-minute window that deduct the amount from the linked billing account (also below zero) or are kept as unattributed usage, organization links with the billing account role check of the user, the `ALLOWED`, `BLOCKED` and `SUSPENDED` billing status of each linked organization with a pollable change sequence, and the billing and current-month usage of one organization. Services authenticate with a Logto machine-to-machine access token with the scope `accounts:report-usage`, and the new optional `USAGE_SERVICE_CLIENTS` configuration maps each client to the services that it may use. Owners and admins get an email when a linked organization is blocked and when it is suspended 3 days later. Members can list the linked organizations and the monthly usage summary with the new `BillingUsageService`.
