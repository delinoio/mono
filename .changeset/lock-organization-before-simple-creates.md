---
"ownsand-api": patch
---

Return `not_found` instead of an internal error when `CreateInvitation`, `CreateEnrollmentToken`, `UpdateAgentVersionPolicy` or `CreateApiKey` waits for a concurrent `DeleteOrganization` of the same organization and the deletion commits. These calls now take a share of the organization row before their insert, as `CreateSandbox` and the other creates do.
