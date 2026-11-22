---
"ownsand-agent": patch
---

The userspace network stack of an `OUTBOUND` sandbox on macOS and Windows now admits at most 256 concurrent guest TCP connections and 256 concurrent guest UDP flows per sandbox, as separate limits per transport. Before, one guest could open unlimited host sockets and goroutines in the shared agent process. A TCP connection beyond the limit gets a reset and a datagram of a new UDP flow beyond the limit is dropped, both before any host socket opens. The stack logs rejections as a structured warning at most once every 5 seconds per transport. A TCP connection whose one direction has ended now also ends after 30 seconds without data in the other direction, so a silent remote side cannot hold the connection forever.
