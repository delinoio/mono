---
"ownsand-api": patch
---

`ownsand-api` now orders pre-release agent versions by SemVer 2.0.0 section 11 precedence. Numeric pre-release identifiers compare by value, so a device on `1.9.0-rc.10` no longer gets an update command when the platform default is `1.9.0-rc.2`, and a device on `1.9.0-rc.2` gets one ordinary update to `1.9.0-rc.10`. A numeric identifier of any length is compared without integer conversion.
