---
"ownsand-api": patch
---

Delete a pending session or relay grant revocation when its session expires or its grant stops being revocable, so an agent that never acknowledges it cannot make the lease holder keep the record while the agent channel stays open.
