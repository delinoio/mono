---
"ownsand-agent": patch
---

Drop every IPv6 packet that a sandbox tap sends into the forward chain of the Linux network policy. Guests get no IPv6 address, but the forward chain only dropped IPv6 packets to the blocked6 prefixes, so a guest that spoofed an IPv6 source could reach any other IPv6 destination through the host. The new rule `iifname "osb-*" meta nfproto ipv6 drop` follows the per-tap IPv4 source guards and precedes the blocked-set drops; the input chain (IPv6 neighbor and router discovery), the IPv4 rules and masquerade are unchanged.
