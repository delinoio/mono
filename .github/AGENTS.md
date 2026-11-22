# GitHub workflow instructions

- Follow root `AGENTS.md` and [the workflow contract](../docs/repository-workflow-contract.md).
- Connect every new PR validation job to `ci-result` through its `needs` list and its `job_outputs` table. Keep `ci-result` running after dependency failures and cancellations so it reports the aggregate result.
- Use the `changes` job outputs to select affected validation. Update the planner inputs when shared validation dependencies change, and include `changes` in `ci-result.needs`.
- Manual validation uses the selected revision comparison. Do not add a full manual mode.
- Set `timeout-minutes` on every job. Follow [the workflow contract](../docs/repository-workflow-contract.md#validation) for the limits.
- Keep every workflow and local action free of actionlint and zizmor findings. After a change, run `go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12` from the repository root; the CI `workflows` job also runs zizmor 1.32.0 with online audits. Suppress a zizmor finding only with an inline `# zizmor: ignore[<audit>]` comment and a comment that explains why the finding does not apply.
- Add release workflows only with an owning project contract and explicit credential boundaries.
- Keep `deploy.yml` a `workflow_dispatch`-only workflow without inputs that selects deployables from pending changesets, builds servers in parallel, deploys in dependency order and pushes the version commit and tags last. Follow [the deployment workflow](../docs/repository-workflow-contract.md#deployment-workflow).
- Keep exactly two GitHub deployment environments: `static-sites` for static sites under `apps/` and `servers` for services under `servers/`. Do not add per-app, per-server, per-stage or per-branch environments. Follow [the workflow contract](../docs/repository-workflow-contract.md#deployment-environments).
