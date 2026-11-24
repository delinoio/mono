---
"tide-cli": patch
---

Allow Tide downloads to replace local files and directories whose names reach the filesystem's filename limit. If replacement fails and Tide cannot restore the previous destination, the error reports where to recover it.
