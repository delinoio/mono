---
"accounts-api": patch
---

Exit cleanly when a shutdown signal cancels startup before serving. Release any bound sockets and skip the public server, billing worker, and metrics listener startup.
