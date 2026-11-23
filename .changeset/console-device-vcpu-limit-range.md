---
"ownsand-console": patch
---

The device limits form rejects a vCPU limit that is not a whole number from 0 to 2147483647 with a field error on the vCPU limit and a toast, and sends no request. Before, the request serializer failed and the form showed only the generic error.
