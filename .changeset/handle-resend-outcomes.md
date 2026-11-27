---
"accounts-api": patch
---

Require a Resend email ID before confirming acceptance. Retry billing emails with missing IDs and revoke invitations when acceptance cannot be confirmed. Finalize billing recipients when Resend confirms that the key was used with a different payload.
