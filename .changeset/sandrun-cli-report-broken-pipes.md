---
"sandrun-cli": patch
---

Return exit status 1 when a closed output pipe prevents SandRun from writing help, version, a GitHub installation URL, or session exec or attach output on Linux and macOS. Report the write error when standard error is writable.
