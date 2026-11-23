---
"ownsand-agent": patch
---

A failed Linux `OUTBOUND` network attach now always removes the DNS accept rules of its tap, also when a refresh that ran independently of the attach (the periodic policy poll or another attach) had already applied them and the attach's own refresh then failed. Before, those rules stayed in the packet filter ruleset until the next refresh. The rules of the other taps stay, and the attach still returns its original error.
