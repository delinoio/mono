---
"ownsand-agent": patch
---

The guest helper on Windows now maps Win32 errors to the documented error codes. Before, it compared POSIX placeholder values that no real Windows error matches. A write to a full disk (`ERROR_DISK_FULL`, `ERROR_HANDLE_DISK_FULL`) now returns `RESOURCE_EXHAUSTED` instead of `INTERNAL`. An exec of a file that is not a valid executable image (`ERROR_BAD_EXE_FORMAT` or `ERROR_EXE_MACHINE_TYPE_MISMATCH`) now fails with `PERMISSION_DENIED`, and an exec that finds no memory to start the process (`ERROR_NOT_ENOUGH_MEMORY`, `ERROR_OUTOFMEMORY`) now fails with `RESOURCE_EXHAUSTED`; both used to report `OTHER`. Linux and macOS guests are unchanged.
