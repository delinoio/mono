---
"ownsand-agent": patch
---

Drop packets to the guest subnet `172.30.0.0/16` that arrive on an interface other than a sandbox tap or `lo` in the input chain of the Linux network policy. The DNS forwarder listens on the gateway addresses without binding to a device, so a LAN peer that routed the guest subnet through the host could query it. The new rule `iifname != "lo" iifname != "osb-*" ip daddr 172.30.0.0/16 drop` follows the per-tap accepts and precedes the final `iifname "osb-*" drop`; guest DNS and host-local queries on `lo` are unchanged.
