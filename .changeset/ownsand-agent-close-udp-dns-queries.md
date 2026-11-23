---
"ownsand-agent": patch
---

Closing a sandbox's DNS forwarder now ends its in-flight UDP queries at once. Before, a UDP query whose resolver stayed silent held `Close` for the rest of its 5-second upstream deadline, because the upstream socket and its dial did not follow the forwarder. Now the dial and the wait for an answer end when the forwarder closes, a socket that a dial returns during the close is closed, and `Close` returns after every admitted UDP query has released its slot. A running forwarder keeps the 5-second dial timeout and the 5-second exchange deadline, and the TCP behavior and the UDP admission limit do not change.
