---
"ownsand-agent": patch
---

`build-kernel.sh` now mounts the kernel source cache by its absolute host path. Before, a relative `OWNSAND_KERNEL_CACHE` without a slash, such as `kernel-cache`, reached Docker unchanged and was read as the name of a Docker named volume, so archives in the host directory were not visible and the archive was downloaded again. The script now creates the directory under the working directory of the caller and resolves it with `cd` and `pwd -P`, which prints the physical path with symbolic links resolved (for example `/private/var` for `/var` on macOS). A cache path that cannot become a directory, for example a regular file, stops the script with a non-zero status before Docker runs. The default `$HOME/.cache/ownsand-kernel`, an absolute override and a path with a space keep mounting their own host directory as one argument.
