---
"delino-accounts": patch
---

Fix the confirmation and form dialogs of the web app. A confirmation dialog disables its confirm button while the action runs, so a double click runs a destructive action once. A form dialog that closes during a submission and opens again starts with an enabled submit button and without the failure of the earlier submission. A failure that arrives after the dialog closed or opened again no longer shows.
