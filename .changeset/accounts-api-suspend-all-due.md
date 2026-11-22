---
"accounts-api": patch
---

Record every due suspension in one call instead of at most 100 billing accounts, so a status poll or a usage report no longer returns a `BLOCKED` organization after its suspension time when more than 100 billing accounts are due.
