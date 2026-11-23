---
"ownsand-agent": patch
---

The guest helper now treats a failed write of `/etc/resolv.conf` as a network tunnel setup failure. Before, it ignored the error and started the tunnel without the gateway as name server, so the guest could not resolve names. Now it closes the TAP device without opening the vsock listener, logs `network tunnel failed` with an error that names `/etc/resolv.conf`, and serves without a tunnel.
