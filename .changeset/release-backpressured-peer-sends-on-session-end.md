---
"ownsand-sdk": patch
---

A peer-to-peer session end (a session-end frame, a channel failure or closing the handle) now releases a sender that waits for the send buffer to drain, and the senders queued behind it, even if the closed channel emits neither `close` nor `bufferedamountlow`. Before, those writes could wait forever. The old channel sends no further fragment, and recoverable writes continue on a replacement session.
