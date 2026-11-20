# ownsand-api

OwnSand control plane served at <https://ownsand.api.delino.io>.

```sh
OWNSAND_DATABASE_URL=... OWNSAND_LOGTO_ISSUER=... pnpm --filter ownsand-api dev
OWNSAND_TEST_DATABASE_URL=... pnpm exec turbo run test --filter=ownsand-api
```

Configuration: [`.env.example`](.env.example). Contract:
[`docs/servers-ownsand-api-foundation.md`](../../docs/servers-ownsand-api-foundation.md).

Licensed under [FSL-1.1-ALv2](../LICENSE).
