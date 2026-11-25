---
"ownsand-agent": patch
---

The Linux guest helper now gives a new filesystem Unix listener lock file mode `0600` even when the umask masks owner permissions. Later starts can reopen that same file.
