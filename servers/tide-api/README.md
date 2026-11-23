# tide-api

tide control plane served at <https://tide.api.delino.io>.

```sh
DATABASE_URL=... LOGTO_ISSUER=... TIDE_SECRET_KEYS=... pnpm --filter tide-api dev
TIDE_TEST_DATABASE_URL=... pnpm exec turbo run test --filter=tide-api
```

The development server listens on `127.0.0.1:18400` (HTTP, h2c) and
`127.0.0.1:19400` (metrics). Postgres-backed tests skip without
`TIDE_TEST_DATABASE_URL`, a Postgres URL whose user may create databases.

Configuration: [`.env.example`](.env.example). Contract:
[`docs/servers-tide-api-foundation.md`](../../docs/servers-tide-api-foundation.md).

Licensed under [FSL-1.1-ALv2](../LICENSE).
