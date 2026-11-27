---
"sandrun-api": patch
---

Fail and clean up a starting runner when OwnSand status checks keep failing past the startup deadline. A bound request keeps its original queue time on retry and ends when it reaches the start attempt limit.
