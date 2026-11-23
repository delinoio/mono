---
"ownsand-agent": patch
---

An `import_snapshot` command now reserves the device against the update installation before it removes a stale copy that a delete marked. During an installation, an import of such a copy fails with `unavailable` and leaves the copy, its directory and its manager entry untouched. Before, the removal ran first and only the later transfer check refused the import. An import that reserved first now holds the installation back through the removal and the import.
