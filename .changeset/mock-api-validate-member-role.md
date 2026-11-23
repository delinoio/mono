---
"ownsand-console": patch
---

The in-browser mock API now rejects an omitted, unspecified, unknown or owner role in `UpdateMemberRole` with `invalid_argument` before it looks up the member, as the control plane does. A valid role for the current owner still fails with `permission_denied`.
