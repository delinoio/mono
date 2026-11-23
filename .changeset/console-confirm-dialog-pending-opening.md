---
"ownsand-console": patch
---

Disable the confirm button of a confirmation dialog while its request runs, so a double click sends the action once, and apply the result of a request only to the opening that sent it, so a late success no longer closes the next confirmation and a late failure no longer shows its error on it.
