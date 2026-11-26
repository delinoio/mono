# Changesets

Pending changesets select the packages that `.github/workflows/deploy.yml` deploys.

Add a changeset to each pull request whose change must be deployed or released:

```sh
pnpm changeset
```

Select every changed package and its bump type, and write a short summary. Do not select `delidev`: Changesets ignores it until its native versions can be updated together. Select a dependent site or server explicitly when it must be rebuilt and deployed; Changesets does not always add dependents.

After the selected site and server deployments and binary builds succeed, the `release` job consumes the selected changesets. It pushes the version commit with the new versions and changelogs to `main` and pushes a `<package>@<version>` tag for each package whose version advances. The `publish-binaries` job publishes the selected binaries after that push, so a publication failure can leave the changesets consumed and the tags in place.

If `publish-binaries` fails, rerun the failed jobs of the original workflow run. A new dispatch plans from the changesets still pending on current `main` and omits changesets the original run already consumed. See [Binary releases](../docs/repository-workflow-contract.md#binary-releases) for recovery when the version push landed but the `release` job failed, or when a manifest upload was interrupted.

See the [deployment workflow contract](../docs/repository-workflow-contract.md#deployment-workflow).
