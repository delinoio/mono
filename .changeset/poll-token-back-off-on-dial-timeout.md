---
"ownsand-cli": patch
"sandrun-cli": patch
---

`login` backs off when the connection to the Logto token endpoint times out (after 10 seconds for the default HTTP client) while the device code is still valid. The poll interval doubles for every such timeout and stays longer for the later polls, instead of ending the login with the transport error. A refused connection, a response timeout and other transport failures still end the login.
