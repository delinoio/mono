---
"ownsand-agent": patch
---

`ownsand-agent reset --yes` now deletes the local data of a damaged state directory. It no longer creates the runtime subdirectories first, so a regular file where `images` belongs no longer makes it exit with `1` before it deletes anything: it removes `identity.json` and every other entry and exits with `0`. A failure to delete one entry still does not stop the others and makes the reset exit with `1`. An absent state directory counts as empty: `reset` does not create it, still runs the host clean-up and exits with `0`. A state directory path that is not a directory, or that cannot be read, is an error. The local data is still deleted when the Firecracker host clean-up fails, and `reset` without `--yes` still changes nothing.
