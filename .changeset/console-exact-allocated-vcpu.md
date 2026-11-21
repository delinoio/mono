---
"ownsand-console": patch
---

Show the exact allocated vCPUs of a device, including totals above 2147483647, from the new 64-bit allocation field, and fall back to the 32-bit count when the server does not send it.
