---
"ownsand-api": patch
---

`ownsand-api` now orders agent versions with a stricter shared `internal/semver` parser. A version that has a sign (`+1.2.3`), a zero-padded component (`01.2.3`) or an empty pre-release suffix (`1.2.3-`) no longer parses, so it is no longer treated as equal to the plain version. When the platform default or the running version of a device is such a string, the server sends no ordinary default target and logs `agent_update_skipped` with `version_not_comparable`. The `ownsand-agent` update path rejects the same target versions.
