---
"sandrun-api": patch
---

Enforce the WatchQueue response deadline when a client stops reading. Clients that keep reading receive OK at normal rotation and can reconnect.
