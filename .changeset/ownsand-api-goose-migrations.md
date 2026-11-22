---
"ownsand-api": patch
---

Apply the database migrations with goose instead of the embedded migration runner. goose records the applied versions in `goose_db_version`. A local database that the earlier runner created (with `schema_migrations`) must be dropped and created again.
