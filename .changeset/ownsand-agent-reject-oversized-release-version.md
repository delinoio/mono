---
"ownsand-agent": patch
---

The `release-manifest` tool now rejects a release version whose component is too large for the agent's version parser, for example `9223372036854775808.0.0`. `add-release` refuses it before it writes any output, including for the first release, and manifest validation refuses it too. Before, only the spelling was checked, so such a version passed the tool and then failed every version comparison. Canonical `MAJOR.MINOR.PATCH` versions still work, and prefixes, pre-release suffixes and leading zeros stay invalid.
