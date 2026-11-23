---
"ownsand-api": patch
---

`WriteFile` now bounds the return of the write result's credit by the call deadline. Before, a credit return that waited for room in a full outbound queue of a stalled agent connection could hold the call, and the queuing of its stream close frame, past the five-minute deadline. The call now fails with `deadline_exceeded` at the deadline, and a caller that canceled first keeps the cancellation.
