---
"ownsand-api": patch
---

Apply the database migrations with goose instead of the embedded migration runner. goose records the applied versions in `goose_db_version`. At the first start, the server copies the history of the earlier runner from `schema_migrations` into `goose_db_version`, so it does not apply those migrations again, and it keeps `schema_migrations` unchanged. The server no longer refuses to start when an applied migration file changed. Do not roll back to an earlier release after goose applied a migration that the earlier release also contains (for example `0009` on a database that was at `0008`); that release then fails to start.
