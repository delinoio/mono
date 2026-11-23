---
"ownsand-sdk": patch
---

Apply the client organization (or `OWNSAND_ORG`) to `client.relay.exec`, `client.relay.attachExec` and `client.relay.readFile` when the request leaves `organizationId` empty or omits it. An explicit request organization is kept. When neither the client nor the request has an organization, consuming the call now fails locally with the Connect code `invalid_argument` before any request is sent. These calls stay server-streaming.
