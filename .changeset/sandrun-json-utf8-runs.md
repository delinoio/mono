---
"sandrun-cli": patch
---

Keep JSON exec and attach output consistent across chunk boundaries by replacing each run of malformed UTF-8 bytes once. Report the byte offset of the first character represented in each output event.
