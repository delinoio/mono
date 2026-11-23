---
"ownsand-cli": patch
"tide-cli": patch
---

On Windows, `logout` now retries the deletion of the credential file for up to 2 seconds while another program holds it open and the deletion fails with access denied or a sharing violation. After that time it exits with an error and keeps the credentials. Linux and macOS delete the file with a single attempt, as before.
