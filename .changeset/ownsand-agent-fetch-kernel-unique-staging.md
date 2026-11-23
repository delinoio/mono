---
"ownsand-agent": patch
---

`fetch-kernel.sh` now downloads the Firecracker guest kernel into a unique temporary file next to the output (`<output>.download.XXXXXX`) and renames it over the output only after the pinned SHA-256 matches. Before, every run used the shared path `<output>.download`, so fetches that overlapped could verify or publish each other's bytes. A failed download or a checksum mismatch now leaves an existing output unchanged, and the script removes its temporary file when it ends, also on `INT` and `TERM`. The published kernel is mode `0644`.
