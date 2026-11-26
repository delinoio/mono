---
"sandrun-api": patch
---

Deploy `sandrun-api` to the shared DigitalOcean Droplet at `https://sandrun.api.delino.io`. The Caddy route proxies HTTP/2 without TLS (h2c) to the container. A deployment run that also deploys `ownsand-api` deploys it before `sandrun-api`, because `sandrun-api` calls the OwnSand API.
