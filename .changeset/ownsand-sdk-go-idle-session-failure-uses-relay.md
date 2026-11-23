---
"ownsand-sdk": patch
---

Go SDK: a peer-to-peer session that fails while no operation uses it (the data channel closes without a session-end frame, the peer connection or ICE fails, or the byte stream is invalid) now moves an automatic handle to the relay. Before, the next operation ignored the cause of the ended session and ran `GetIceServers` and `OpenSession` again, so every operation on that handle could retry direct setup. Now the next and later operations use the relay with no further `GetIceServers` or `OpenSession` call. A session-end frame or a retirement of an idle session still keeps direct setup available, and a handle with the transport forced to peer-to-peer still opens a new session. A late failure of a session that another session already replaced no longer changes the route.
