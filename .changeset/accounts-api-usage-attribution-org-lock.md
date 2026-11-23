---
"accounts-api": patch
---

Serialize an attributed usage report with the creation of a link of its organization: the report now takes the organization lock after its billing account lock and holds both until it commits, so a link to another billing account can no longer commit between the report's last attribution read and its insert.
