---
"ownsand-agent": patch
---

Scope the DHCPv4, DHCPv6 and IPv6 neighbor and router discovery accepts of the Linux network policy input chain to one sandbox tap each. They matched every `osb-*` interface with only a port or ICMPv6 type, so a guest could reach UDP port 67 or 547 on any host address from any source address, or send discovery messages that RFC 4861 rejects. The agent now renders, for each attached sandbox in tap order after the DNS rules, DHCPv4 from `0.0.0.0` or the guest address to the gateway address or `255.255.255.255`, DHCPv6 from `fe80::/10` to `ff02::1:2`, and neighbor and router discovery with hop limit 255 to `fe80::/10` or `ff02::/16`; the final `iifname "osb-*" drop` stays last and the forward chain, source guards and masquerade are unchanged.
