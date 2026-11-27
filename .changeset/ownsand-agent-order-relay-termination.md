---
"ownsand-agent": patch
---

Keep an OwnSand relay stream's terminal frame last. A caller close or error now stops pending output and acceptance notices from entering the queue. If the agent needs to end a stream and the queue is full, it closes the relay connection so blocked sends can return.
