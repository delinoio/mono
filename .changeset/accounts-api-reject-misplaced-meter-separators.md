---
"accounts-api": patch
---

Accept only the exact canonical key of a meter when `ListUsageSummary` decodes a stored meter key. A stored key whose separator is in the wrong place, such as `ownsand_relay.egress`, no longer decodes as `METER_OWNSAND_RELAY_EGRESS`; its row now has `METER_UNSPECIFIED`, while the row of the canonical key `ownsand.relay_egress` keeps its meter. Row quantities, row amounts and the account total do not change.
