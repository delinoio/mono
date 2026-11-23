---
"delino-accounts": patch
---

The invitation form now shows the server message `too many invitations; try again later` when `CreateInvitation` fails with `resource_exhausted` because the inviter or the address reached its 24-hour invitation limit. The dialog stays open with the entered address. Other `resource_exhausted` errors still show "Too many requests. Wait a moment and try again."
