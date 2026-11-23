---
"ownsand-agent": patch
---

The `release-manifest` tool now parses the URLs it writes and signs. `add-release` refuses a `--url-prefix` that is hostless (`https:///releases`), malformed (a space in the host, an invalid percent escape or port) or has a query or fragment, including an empty `?` or `#`, before it writes the manifest. Manifest validation, which `sign` runs before it emits a signature, refuses a hostless or malformed artifact URL. Before, only the `https://` text prefix was checked. Prefixes with or without a trailing slash, escaped tag paths such as `ownsand-agent%401.2.3` and complete artifact URLs with a query still work.
