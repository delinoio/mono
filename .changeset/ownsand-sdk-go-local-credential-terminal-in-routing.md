---
"ownsand-sdk": patch
---

Keep Go peer-to-peer routing when the token source fails: a callback error or an empty token during session setup now ends the operation without an `AuthorizeRelay` call or a move to the relay, and a later operation sets the session up again.
