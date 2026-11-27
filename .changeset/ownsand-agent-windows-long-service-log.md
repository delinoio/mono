---
"ownsand-agent": patch
---

Keep Windows service file logging available when the state directory puts `agent.log` beyond `MAX_PATH` and Windows long-path opt-in is disabled.
