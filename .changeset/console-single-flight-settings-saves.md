---
"ownsand-console": patch
---

Accept one save at a time in the agent version policy form and the device limits form. The save button and the fields stay disabled until the save and its refetch have settled, so a second submit cannot start and the refetch cannot erase an edit made after the submit. A failed save keeps the draft and re-enables the form.
