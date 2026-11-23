---
"ownsand-api": patch
---

Fail a snapshot export whose reported part numbers do not form the contiguous set 1..N (a gap, or a first number above 1) as `EXPORT_FAILED` with reason `invalid_part` and a clean-up job for the attempt, instead of marking the snapshot `EXPORTED` although it cannot be restored. A contiguous manifest still reaches `EXPORTED` in any reported order.
