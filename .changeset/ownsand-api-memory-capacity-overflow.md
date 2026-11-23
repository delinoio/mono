---
"ownsand-api": patch
---

The scheduler no longer adds the memory that a device reports as allocated to the memory of a request when it checks the remaining capacity. Before, an allocation near the largest 64-bit integer wrapped the sum to a negative number and the device passed the check. Now the check compares the allocation with the device limit minus the request, so an allocation that leaves too little memory fails with `unavailable` ("running sandboxes hold the memory capacity") for every valid value, an exact fit still passes, and a request above the device memory limit still fails with `failed_precondition`.
