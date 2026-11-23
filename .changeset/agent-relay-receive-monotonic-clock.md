---
"ownsand-agent": patch
---

The agent now measures relay connection receive inactivity with monotonic elapsed time. The receive timeout and the echo probe no longer change when the system wall clock steps forward or back: a forward step no longer drops a healthy relay connection, and a backward step no longer delays the detection of a lost one. Any received byte, also bytes of an incomplete frame, still restarts the interval. The timeouts keep their values.
