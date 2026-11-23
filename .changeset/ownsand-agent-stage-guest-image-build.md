---
"ownsand-agent": patch
---

`cmds/ownsand-agent/images/linux/build-image.sh` no longer builds the guest helper into `<output-dir>/ownsand-guest` and then deletes that path. It builds into a unique staging directory inside the output directory, mounts it read-only into the root file system container, and removes it on success, on a failed Go, root file system or kernel step, and on `INT` and `TERM`. A file that already exists at `<output-dir>/ownsand-guest` stays unchanged byte for byte, and the outputs `vmlinux` and `rootfs.ext4` are the same.
