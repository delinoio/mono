---
"accounts-api": minor
---

Add shared billing accounts with the `owner`, `admin` and `viewer` roles, email invitations through Resend and ownership transfer as the Connect service `BillingAccountService`. The server now stores its data in Postgres with embedded goose migrations, validates Logto access tokens with the scope `accounts:billing`, reads the primary email of an invited user through the Logto Management API and needs the new `DATABASE_URL`, `LOGTO_*`, `RESEND_API_KEY` and `EMAIL_FROM` configuration.
