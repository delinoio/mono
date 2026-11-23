---
"ownsand-api": patch
"ownsand-agent": patch
---

Reject a malformed pre-release suffix when parsing a version. Each dot-separated identifier must be non-empty, use only ASCII letters, digits and `-`, and an all-digit identifier must not have a leading zero, so `1.2.3-rc.` and `1.2.3-rc.01` no longer parse and `1.2.3-rc.` no longer compares equal to `1.2.3-rc`. A malformed `revoked_versions` or release entry in the agent update manifest can no longer match a valid version, and the server skips the default update when a version is malformed.
