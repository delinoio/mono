---
"ownsand-agent": patch
---

`fetch-kernel-source.sh` now fails with a non-zero status that names the path when the archive path `linux-<version>.tar.xz` in the kernel cache is a directory. Before, it moved the downloaded archive into that directory under a temporary name, left it there and reported success. The directory and its contents stay unchanged and no temporary download is left.
