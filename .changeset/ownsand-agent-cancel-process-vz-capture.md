---
"ownsand-agent": patch
---

Stop in-progress process and Virtualization.framework snapshot copies when the agent channel closes. The agent removes unfinished snapshots and resumes any VM it paused for capture. A native copy or memory save without an interrupt API can finish before the agent cleans up.
