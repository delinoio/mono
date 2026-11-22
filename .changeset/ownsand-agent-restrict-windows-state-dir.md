---
"ownsand-agent": patch
---

On Windows, the agent now restricts its state directory to `NT AUTHORITY\SYSTEM` and `BUILTIN\Administrators` with a protected ACL that its content inherits, so other local accounts can no longer read the device key. Before, only the POSIX mode was set, which Windows ignores, and the directory kept the default `%ProgramData%` permissions. An existing state directory gets the same ACL at the next start, and the agent refuses a state directory that another account owns.
