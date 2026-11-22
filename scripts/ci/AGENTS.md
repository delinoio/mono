# CI script instructions

- Follow root `AGENTS.md` and [the workflow contract](../../docs/repository-workflow-contract.md).
- Pass the exact base/head comparison to formatting checks.
- Keep staged formatting independent of HEAD so initial commits work.
- Preserve unstaged edits when hooks format staged files.
- Keep the staged formatting hook supporting `git add` plus `git commit`, `git commit -a` and `git commit -i`. Never write or stage formatting fixes while `GIT_INDEX_FILE` is a throwaway `next-index-*.lock` index (pathspec, `--only` and `--amend <path>` commits); check without writing and refuse with guidance instead.
- Resolve paths against the repository root (`git rev-parse --show-toplevel`), never the invoking directory, in every selector and formatter run.
- Keep `imported-skills.manifest.json` equal to the checked-in imported skill directories: after a deliberate change to a file of an imported skill, run `node scripts/ci/imported-skills.mjs --write-manifest` and commit the manifest with it. The check never writes `skills-lock.json`; only the `skills` CLI does.
