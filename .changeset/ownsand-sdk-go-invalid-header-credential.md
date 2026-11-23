---
"ownsand-sdk": patch
---

Go SDK: a fixed API key, the `OWNSAND_API_KEY` key or a `TokenSource` result that is not a valid HTTP header value (for example one with a newline or another control character) now ends the operation at once with a terminal local credential error. The error message names the problem and never contains the credential. Before, Go's HTTP transport rejected the request on every attempt, and the SDK took the rejection for a transport interruption: `CreateSandbox` and `AuthorizeRelay` retried until their windows ended, and automatic mode moved the handle to the relay. Now no request reaches the HTTP transport, no retry, recovery or `AuthorizeRelay` call starts, and the handle stays eligible for peer-to-peer setup with a later valid token. Valid credentials and retries after real transport interruptions behave as before.
