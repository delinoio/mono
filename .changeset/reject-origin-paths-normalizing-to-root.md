---
"ownsand-sdk": patch
---

`new OwnSandClient` now rejects an API origin whose path only reads as the root after the URL parser removes dot segments, such as `https://example.test/subdir/..` or `https://example.test/.`. It throws `OwnSandConfigError` ("origin must not have a path, query or fragment") without echoing the supplied URL. Bare `http` and `https` origins, explicit ports, a single trailing slash and the default origin stay valid.
