---
"ownsand-agent": patch
---

`ownsand-agent service print` and `service install` now put an explicit `--state-dir` into the service definition, also when `OWNSAND_AGENT_STATE_DIR` names another directory. Before, the flag was dropped and the service ran with the state directory of the environment or the host default. Without the flag the definition is unchanged.
