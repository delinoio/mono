# CI script instructions

- Follow root `AGENTS.md` and [the workflow contract](../../docs/repository-workflow-contract.md).
- Pass the exact base/head comparison to formatting checks.
- Keep staged formatting independent of HEAD so initial commits work.
- Preserve unstaged edits when hooks format staged files.
- Keep the staged formatting hook supporting `git add` plus `git commit`, `git commit -a` and `git commit -i`. Never write or stage formatting fixes while `GIT_INDEX_FILE` is a throwaway `next-index-*.lock` index (pathspec, `--only` and `--amend <path>` commits); check without writing and refuse with guidance instead.
