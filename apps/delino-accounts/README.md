# delino-accounts

Account and billing web app served at <https://accounts.delino.io>.

```sh
pnpm --filter delino-accounts dev
pnpm --filter delino-accounts build
pnpm --filter delino-accounts test
```

Build-time configuration: [`.env.example`](.env.example). TypeScript bindings come from the workspace package `@delino/accounts-protos`.

Contract: [`docs/apps-delino-accounts-foundation.md`](../../docs/apps-delino-accounts-foundation.md).
