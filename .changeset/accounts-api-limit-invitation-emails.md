---
"accounts-api": minor
---

`CreateInvitation` now limits invitation volume. One inviter can create at most 30 invitations, and one email address can receive at most 5, in any 24 hours. Both limits count across all billing accounts and every invitation state, so a revoke, a failed send or a new billing account does not reset them. The call that would exceed a limit fails with `resource_exhausted` and the message `too many invitations; try again later`, stores nothing and sends no email, and the server logs `invitation_rate_limited` with the billing account, the inviter and the limit kind but never the address. A pending duplicate still fails with `already_exists`. Migration `00009_invitation_rate_indexes.sql` adds the two supporting indexes; it only adds indexes and is compatible with the previous release.
