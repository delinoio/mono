---
"ownsand-agent": patch
---

The agent now reports the update state `REVOKED_PENDING_IDLE` with the target version as soon as the verified manifest revokes the running version, including while the artifact download is still pending, and not only after the download finishes. A failed installation on a revoked running version also keeps `REVOKED_PENDING_IDLE` with the failed target version, removes the downloaded artifact and returns the original install error, where before it reported `FAILED`. A running version that is not revoked keeps `DOWNLOADING` and `FAILED`, and a manifest with an invalid signature never starts the gate.
