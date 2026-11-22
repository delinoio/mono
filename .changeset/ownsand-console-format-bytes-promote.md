---
"ownsand-console": patch
---

Fix the console byte sizes that showed as `1024.0 KiB`, `1024.0 MiB` or `1024.0 GiB`. A size that rounds to 1024.0 at one decimal now shows in the next unit, for example `1.0 MiB` for 1,048,575 bytes.
