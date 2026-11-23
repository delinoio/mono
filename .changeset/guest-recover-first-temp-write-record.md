---
"ownsand-agent": patch
---

The guest helper now recovers the first write record of a staged write when a crash left it as a temporary file. Before, a crash between the sync and the rename of the first record save left only `<hash>.json.tmp`, and the next start removed it. The staging file then stayed untracked and never expired, and a retry of the same write ID failed because the staging file already existed. The helper now renames a complete, valid temporary record to its canonical name when no canonical record exists, so the staging file is tracked and expires like any recovered write. A temporary file next to a canonical record, and any malformed, wrong-name or unsupported-version temporary file, is removed.
