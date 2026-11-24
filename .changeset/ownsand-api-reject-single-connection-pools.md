---
"ownsand-api": patch
---

Require at least two Postgres pool connections at startup so sandbox pause and resume can finish while an ordering lock holds one connection. If `DATABASE_URL` sets `pool_max_conns=1`, raise it to at least 2 before deploying.
