---
"ownsand-api": patch
---

`OpenSession` and `AuthorizeRelay` now check an API key and its organization in the database again after they register the pending session or grant. Before, the second authorization read no key state, so a key that was revoked (or an organization that was deleted) after the request was authenticated and before the registration was not rejected: its revocation event found nothing to end, and the call sent an offer or a grant to the agent. Such a call now fails with `unauthenticated` and leaves no session or new grant.
