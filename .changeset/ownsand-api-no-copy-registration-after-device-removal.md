---
"ownsand-api": patch
---

Stop an import result that is handled after a device removal from registering a snapshot copy row for the removed device. The result now locks the device row before it inserts the row and writes nothing when the device is removed.
