---
"ownsand-agent": patch
---

The guest helper converts the time of a `SetClock` request to the `timeval` of `settimeofday` from the Unix seconds and the sub-second part separately. A time after 2262-04-11 (for example 2300-01-01) no longer overflows the 64-bit nanosecond count and sets a wrapped time, such as 1715-06-13 for 2300-01-01 on 64-bit Linux. The microseconds still round up, and 1,000,000 microseconds now carry into the seconds. On a guest whose `timeval` fields are narrower than the value, such as the 32-bit seconds of 32-bit Linux, the helper returns an error and does not set the clock, instead of setting a truncated time. The error mapping of a rejected `settimeofday` call and the `ignore` clock mode are unchanged.
