---
"ownsand-api": patch
---

Billing now keeps its pause mark of a sandbox for the rest of the sandbox's life, until a caller's `PauseSandbox` takes it over or the sandbox ends. Before, a confirmed recovery cleared the mark of a sandbox that was still `RUNNING`, and a successful automatic resume cleared it as well. A pause command that billing had sent could still finish on the agent after the API stopped waiting for it and report `PAUSED` for a sandbox without a mark, and no recovery resumed it. The recovery now leaves a marked `RUNNING` sandbox alone (no command, write or log), resumes every marked `PAUSED` sandbox, and clears the mark only of an ended (`DELETED` or `FAILED`) sandbox. `billing_sandbox_unmarked` is logged only for that cleanup. A mark that an earlier version already cleared cannot be reconstructed, so such a late pause stays paused until a caller resumes it.
