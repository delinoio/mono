---
"sandrun-cli": patch
---

Generate guest file upload IDs after the first chunk is ready so slow local reads do not expire the ID before the upload starts.
