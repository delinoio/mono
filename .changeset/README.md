# Changesets

Pending changesets select the packages that `.github/workflows/deploy.yml` deploys.

Add a changeset to each pull request whose change must be deployed or released:

```sh
pnpm changeset
```

Select every changed package and its bump type, and write a short summary. Select a dependent site or server explicitly when it must be rebuilt and deployed; Changesets does not always add dependents. The deployment workflow consumes the changesets after a successful deployment, commits the new versions and changelogs to `main` and pushes a `<package>@<version>` tag for each package whose version advances.

See the [deployment workflow contract](../docs/repository-workflow-contract.md#deployment-workflow).
