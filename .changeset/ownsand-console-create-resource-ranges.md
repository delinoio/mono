---
"ownsand-console": patch
---

Check the vCPU, memory and disk ranges of the create sandbox dialog before the request. A vCPU count above 2147483647, or a memory or disk value above 9007199254740991, now shows a field error and sends nothing, instead of a generic error notice or a rounded value.
