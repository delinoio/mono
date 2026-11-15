# CI script instructions

- Follow root `AGENTS.md` and [the workflow contract](../../docs/repository-workflow-contract.md).
- Pass the exact base/head comparison to formatting checks.
- Keep staged formatting independent of HEAD so initial commits work.
- Preserve unstaged edits when hooks format staged files.
- Resolve paths against the repository root (`git rev-parse --show-toplevel`), never the invoking directory, in every selector and formatter run.
