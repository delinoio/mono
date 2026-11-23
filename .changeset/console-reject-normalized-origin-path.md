---
"ownsand-console": patch
---

Reject a `PUBLIC_API_ORIGIN` or `PUBLIC_LOGTO_ENDPOINT` whose written form is not `scheme://host[:port][/]`, because the URL parser accepts other slash counts after the scheme and resolves dot segments, which hid a path (`https:///host/a/..`) behind a bare origin.
