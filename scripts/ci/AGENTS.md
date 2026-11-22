# scripts/ci working instructions

- Follow the root instruction-update policy and the nearest parent instructions.
- This file covers `scripts/ci/` and its descendants unless a more specific instruction file applies.
- Read the owning contracts below before changing behavior, including affected cross-domain consumers.

## Development and validation

- Follow root `AGENTS.md` and [the workflow contract](../../docs/repository-workflow-contract.md).
- Pass the exact base/head comparison to formatting checks.
- Keep staged formatting independent of HEAD so initial commits work.
- Preserve unstaged edits when hooks format staged files.
- Keep the staged formatting hook supporting `git add` plus `git commit`, `git commit -a` and `git commit -i`. Never write or stage formatting fixes while `GIT_INDEX_FILE` is a throwaway `next-index-*.lock` index (pathspec, `--only` and `--amend <path>` commits); check without writing and refuse with guidance instead.
- Resolve paths against the repository root (`git rev-parse --show-toplevel`), never the invoking directory, in every selector and formatter run.

## Owning contracts

- [Repository workflow](../../docs/repository-workflow-contract.md)
