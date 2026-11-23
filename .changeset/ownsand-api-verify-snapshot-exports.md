---
"ownsand-api": patch
---

`ownsand-api` now verifies each exported snapshot against R2. The control plane bills snapshot storage from the part sizes that the agent reports. After the upload URLs of an export attempt expired (15 minutes), a lease-holder worker lists the attempt prefix and compares the objects with the part manifest. A matching export is marked verified. An export with a missing part, a size difference or an unlisted object becomes `EXPORT_FAILED`, loses its part manifest (so the storage usage stops) and gets an R2 clean-up job. An R2 failure never fails an export. A forward migration adds `snapshots.export_verified` and `snapshots.export_verify_after`; snapshots exported before the migration are verified once, at least 15 minutes after the worker first sees them. No RPC or agent change.
