# sandrun-api

SandRun control plane served at <https://sandrun.api.delino.io>.

```sh
DATABASE_URL=... LOGTO_ISSUER=... SANDRUN_SECRET_KEYS=... pnpm --filter sandrun-api dev
SANDRUN_TEST_DATABASE_URL=... pnpm exec turbo run test --filter=sandrun-api
```

The development server listens on `127.0.0.1:18400` (HTTP, h2c) and
`127.0.0.1:19400` (metrics). Postgres-backed tests skip without
`SANDRUN_TEST_DATABASE_URL`, a Postgres URL whose user may create databases.

Configuration: [`.env.example`](.env.example). Contract:
[`docs/servers-sandrun-api-foundation.md`](../../docs/servers-sandrun-api-foundation.md).

Licensed under [FSL-1.1-ALv2](../LICENSE).
