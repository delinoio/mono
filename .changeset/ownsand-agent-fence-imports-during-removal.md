---
"ownsand-agent": patch
---

The device removal now fences snapshot imports. A snapshot import that is still running when the clean-up starts either publishes before the clean-up boundary, and the clean-up deletes it, or is refused and removes its partial content, so no import creates snapshot metadata or a snapshot directory after the clean-up. A snapshot import that starts after the removal began is refused with `FAILED_PRECONDITION`.
