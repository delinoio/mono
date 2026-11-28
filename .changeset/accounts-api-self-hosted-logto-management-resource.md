---
"accounts-api": patch
---

Add the optional `LOGTO_MANAGEMENT_RESOURCE` setting for the API resource indicator of the Logto Management API. It defaults to `<LOGTO_MANAGEMENT_ENDPOINT>/api`, which is correct for Logto Cloud. A self-hosted Logto names the Management API of its default tenant `https://default.logto.app/api`, so `AcceptInvitation` and member display names could not get a Management API token without this setting.
