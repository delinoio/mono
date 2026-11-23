---
"ownsand-console": patch
---

Make the mock API page `ListSandboxes` and `ListSnapshots` like the control plane and filter `ListSnapshots` by `device_id`. The mock now lists in ascending ID order with 50 rows by default and at most 200, returns a token after every full page, and fails with `invalid_argument` for a negative page size or a malformed page token. Its page tokens now use the opaque control plane form, so a token that names a deleted row keeps working. The Sandboxes page "Show more" works against the mock, and a created sandbox is listed last.
