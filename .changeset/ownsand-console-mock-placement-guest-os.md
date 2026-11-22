---
"ownsand-console": patch
---

Make the mock control plane of the console place a new sandbox only on a device that advertises the guest OS of the source and has built the requested base image, and keep the guest OS of a base image, a snapshot or a template on the sandbox and its snapshot instead of always using Linux.
