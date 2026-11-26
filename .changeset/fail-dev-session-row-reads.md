---
"sandrun-api": patch
---

Return an error when a database read fails partway through loading dev sessions. Roll back member removal if it cannot read the member's open sessions. After a successful removal, log cleanup read errors and still delete the sandboxes already found.
