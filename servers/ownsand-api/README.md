# ownsand-api

OwnSand control plane served at <https://ownsand.api.delino.io>.

```sh
pnpm --filter ownsand-api dev
pnpm exec turbo run test --filter=ownsand-api
```

The server needs Postgres (`DATABASE_URL`). Storage tests and the launcher
tests that start a server run only when `OWNSAND_TEST_DATABASE_URL` names a
Postgres server where the test user may create databases, for example:

```sh
docker run --rm -d --name ownsand-api-pg -e POSTGRES_PASSWORD=test -p 127.0.0.1:55432:5432 postgres:17
OWNSAND_TEST_DATABASE_URL='postgres://postgres:test@127.0.0.1:55432/postgres?sslmode=disable' \
  pnpm exec turbo run test --filter=ownsand-api
```

Contract: [`docs/servers-ownsand-api-foundation.md`](../../docs/servers-ownsand-api-foundation.md).

Licensed under [FSL-1.1-ALv2](../LICENSE).
