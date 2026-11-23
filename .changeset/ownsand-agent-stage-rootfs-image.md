---
"ownsand-agent": patch
---

Make `images/linux/build-rootfs.sh` build the ext4 image as a staged file next to the output and publish it with one rename. A failed allocation or format now leaves an existing image unchanged and removes the staged file, and a failed first build leaves no image at the output path. Before this change the script deleted the output before it allocated and formatted the new image.
