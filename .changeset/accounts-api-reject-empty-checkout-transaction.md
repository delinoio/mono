---
"accounts-api": patch
---

Reject a Paddle checkout transaction answer that has no transaction ID. Before, a success answer such as `{"data":{}}` stored an empty transaction ID as the open checkout of the subscription. The start call now fails with `unavailable` and keeps no checkout row, so the next call asks Paddle again.
