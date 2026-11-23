---
"ownsand-agent": patch
---

Grow a Firecracker sandbox disk by extending the image file only and stop running e2fsck and resize2fs on a guest-controlled disk image on the host; the guest helper grows the root file system online at boot.
