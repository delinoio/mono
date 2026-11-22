---
"delino-accounts": patch
---

The billing account name check now removes the same leading and trailing white space as the API server (the set of Go's `strings.TrimSpace`). Before, it used `String.prototype.trim`, which keeps U+0085 and removes U+FEFF. A name with a leading U+0085 is now trimmed and accepted like the server does, and a leading or trailing U+FEFF is kept and counted in the 100-character limit.
