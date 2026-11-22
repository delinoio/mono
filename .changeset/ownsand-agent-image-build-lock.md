---
"ownsand-agent": patch
---

`ownsand-agent image build` now holds an exclusive lock on the image destination (`images/<os>.build.lock`), so a second build of the same guest OS fails at once with `build in progress for <os> image` instead of deleting the staging directory of the first build. The operating system releases the lock when the first build ends, also when it is killed.
