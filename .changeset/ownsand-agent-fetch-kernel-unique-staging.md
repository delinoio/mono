---
"ownsand-agent": patch
---

`fetch-kernel.sh` now downloads the Firecracker guest kernel into a unique staging directory named `<output>.download.XXXXXX` beside the output. The staged file has the same basename as the output. For example, for `images/linux/vmlinux`, the staged file is `images/linux/vmlinux.download.XXXXXX/vmlinux`, with a generated suffix in place of `XXXXXX`. The previous shared staging path, `<output>.download`, let overlapping fetches verify or publish each other's bytes.

The script verifies the pinned SHA-256 before replacing the output. A failed download or checksum mismatch leaves any existing output unchanged. The script removes the staging directory on exit, including after `INT` or `TERM`. The published kernel has mode `0644`.
