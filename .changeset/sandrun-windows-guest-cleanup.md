---
"sandrun-cli": patch
---

Report temporary file removal failures on Windows dev sessions. Sync still attempts every removal and warns if clean-up fails after the manifest is saved. Directory download logs a clean-up failure and keeps the downloaded files.
