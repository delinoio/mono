# Changesets

Pending changesets select the packages that `.github/workflows/deploy.yml` deploys.

Add a changeset to each pull request whose change must be deployed or released:

```sh
pnpm changeset
```

Select every changed package and its bump type, and write a short summary. The deployment workflow consumes the changesets after a successful deployment, commits the new versions and changelogs to `main` and pushes a `<package>@<version>` tag for each release.

See the [deployment workflow contract](../docs/repository-workflow-contract.md#deployment-workflow).
