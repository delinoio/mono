---
"accounts-api": patch
---

Answer `400` with the log reason `body_read_failed` when the server cannot read the body of a Paddle webhook to its end, such as after a broken connection or a truncated body. Only a body over 1 MiB gets `413` with `body_too_large`, as before.
