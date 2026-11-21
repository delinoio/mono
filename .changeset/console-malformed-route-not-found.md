---
"ownsand-console": patch
---

Show the Page not found screen for a console path with a malformed percent escape, such as `/orgs/%/sandboxes`, instead of crashing the app with a `URIError`.
