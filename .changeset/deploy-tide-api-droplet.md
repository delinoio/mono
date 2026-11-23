---
"tide-api": patch
---

Deploy `tide-api` to the shared DigitalOcean Droplet at `https://tide.api.delino.io`. The Caddy route proxies HTTP/2 without TLS (h2c) to the container. A deployment run that also deploys `ownsand-api` deploys it before `tide-api`, because `tide-api` calls the OwnSand API.
