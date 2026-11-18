# servers-accounts-api foundation contract

## Scope

This contract owns the `accounts-api` component of [`delino-accounts`](project-delino-accounts.md) at `servers/accounts-api/`:

- `servers/accounts-api/cmd/accounts-api/`: process entry point.
- `servers/accounts-api/internal/config/`: environment configuration loading and validation.
- `servers/accounts-api/internal/server/`: HTTP handler, routes, middleware and the connection observer that logs requests rejected before the handler runs.
- `servers/accounts-api/.env.example`: configuration names and safe loopback defaults.
- `servers/accounts-api/package.json` and `turbo.json`: workspace task wiring and the `deploy` field.
- `servers/accounts-api/Dockerfile` and `railway.json`: Railway runtime image and service configuration.
- `servers/accounts-api/scripts/`: development launcher (`dev.mjs`) and its process-level tests.

The API server is the system of record for account and billing information. It is licensed under `FSL-1.1-ALv2` according to the [license contract](repository-license-contract.md).

## Runtime and Language

- Language: Go, in the repository root module `github.com/delinoio/mono`. The import path of the server packages is `github.com/delinoio/mono/servers/accounts-api/...`. The `go` directive in the root `go.mod` selects the toolchain version.
- Runtime: one long-running HTTP server process. It uses only the Go standard library in the current scaffold.
- Logging: `log/slog`.
- HTTP timeouts: `newServer` in `cmd/accounts-api` sets fixed constants on the `http.Server`. They are not configuration variables, so they have no `.env.example` entry and no Turbo `passThroughEnv` entry.
  - `ReadHeaderTimeout` is 10 seconds: the time a client has to send the request headers.
  - `ReadTimeout` is 30 seconds and covers the whole request read, headers and body. `net/http` clears the header deadline after the headers arrive, so without `ReadTimeout` a client that declares a body and then sends nothing would hold the connection open forever. `ReadTimeout` must stay greater than or equal to `ReadHeaderTimeout`.
  - `WriteTimeout` is 30 seconds for writing a response.
  - `IdleTimeout` is 120 seconds: a keep-alive connection that is waiting for the next request is closed after this time.
  - `WriteTimeout` limits the lifetime of every response, so a long-lived streaming RPC must lift or extend it for that handler (`http.ResponseController.SetWriteDeadline`) in the change that adds the stream.
- Deployment target: the Railway service `accounts-api`, deployed by the [deployment workflow](repository-workflow-contract.md#deployment-workflow). The workflow builds a static `linux/amd64` binary with `CGO_ENABLED=0`; `servers/accounts-api/turbo.json` passes `CGO_ENABLED`, `GOOS` and `GOARCH` through to the uncached `build` task. The `Dockerfile` copies `dist/accounts-api`, the FSL-1.1-ALv2 `LICENSE` (`/usr/share/licenses/accounts-api/LICENSE`) and the Go toolchain BSD-3-Clause notice (`/usr/share/licenses/accounts-api/GO-LICENSE`) into the digest-pinned `gcr.io/distroless/static-debian12:nonroot` image and runs it as the `nonroot` user. `railway.json` selects the Dockerfile builder, checks `GET /healthz` before Railway switches traffic and restarts the process on failure up to 10 times.
- Configuration source: Railway service variables of the production environment. Set `ADDR` to `:<port>` and `PORT` to the same port, and set the target port of the public domain to that port. The server reads only `ADDR`, but the Railway health check of `/healthz` uses `PORT`. Secrets are also Railway service variables; they never enter the repository or the GitHub environment.
- TLS termination: the Railway edge terminates TLS for `accounts.api.delino.io` and forwards plain HTTP to the process.

## Users and Operators

- The [`delino-accounts-web`](apps-delino-accounts-foundation.md) app, which calls the API on behalf of signed-in users.
- Other Delino services. The repository rules require every service to use this API for shared account data and for billing, including billable usage or charge reports (`planned`). Record each consuming service and the RPCs that it uses here before it calls the API.
- Operators who deploy and monitor the server.

## Interfaces and Contracts

- Production origin: `https://accounts.api.delino.io`.
- Operational endpoints:

| Endpoint ID | Method and path | Authentication | Response                                                                |
| ----------- | --------------- | -------------- | ----------------------------------------------------------------------- |
| `healthz`   | `GET /healthz`  | none           | `200` with `{"status":"ok"}` while the process can serve HTTP requests. |

- Business APIs (`planned`): Connect RPC services generated from `protos/delino/accounts/v1/`. Planned services:

| Service ID       | Responsibility                                                                        |
| ---------------- | ------------------------------------------------------------------------------------- |
| `AccountService` | Read and update the profile and preferences of the signed-in Logto user.              |
| `BillingService` | Manage payment methods, billing contacts, balances, credits and invoices of the user. |

- Authentication (`planned` with the first business API): the server is the Logto API resource `https://accounts.api.delino.io`. It validates every access token according to the [authentication contract](repository-auth-contract.md) and checks the scopes that each RPC requires. Record the scope names in this table when the RPCs are added.
- Browser access (`planned` with the first business API): CORS allows only the `https://accounts.delino.io` origin in production and the loopback development origin locally.
- Identifiers: persisted entities use UUID v7. User-owned records reference the Logto user ID (`sub`).
- Money: all amounts are `int64` micro-USD values with a `_usd_micros` suffix, according to the [billing contract](repository-billing-contract.md). Record the payment provider conversion and its rounding direction here when the provider is selected.

## Configuration

| Variable    | Required | Default          | Values                           |
| ----------- | -------- | ---------------- | -------------------------------- |
| `ADDR`      | no       | `127.0.0.1:8100` | `host:port` listen address       |
| `LOG_LEVEL` | no       | `info`           | `debug`, `info`, `warn`, `error` |

- The server validates the configuration at startup and exits with a non-zero status when a value is not valid.
- `ADDR` accepts an empty host (all interfaces), an IP address (IPv6 in brackets, with an optional zone) or an RFC 1123 hostname; configuration validation checks only the host syntax, so a hostname that does not resolve still fails when the server starts listening.
- Root `pnpm dev` and `pnpm exec turbo run dev --filter=accounts-api` pass `ADDR` and `LOG_LEVEL` from the shell to the server through Turbo `passThroughEnv` in `servers/accounts-api/turbo.json`. Add every new configuration variable to that list in the change that makes the server read it.
- Production must set `ADDR` explicitly for its deployment target. The default listens on loopback only.
- Logto settings (`planned`): the tenant endpoint and the API resource indicator. Add them to this table and to `.env.example` in the same change that adds token validation.

## Storage

The scaffold has no persistent storage. The database is PlanetScale Postgres, according to the repository defaults. Before the first persisted entity, record the migration tool and the backup policy in this contract, and add the database stage that runs before the servers to the [deployment workflow](repository-workflow-contract.md#deployment-workflow). File uploads, such as invoice documents, use Cloudflare R2 with signed URLs according to the repository defaults unless this contract documents another choice.

## Security

- Accept only Logto access tokens as API credentials. Do not accept ID tokens.
- Payment card data stays with the payment provider. The server stores provider references, not raw card numbers.
- The server never logs tokens, authorization codes, application secrets or payment details.
- Secrets, such as Logto Management API credentials and payment provider keys, follow the [environment contract](repository-environment-contract.md). They never appear in `.env.example`.

## Graceful Shutdown

- `SIGINT` or `SIGTERM` stops the listener from accepting new connections and starts a drain that waits at most 10 seconds (`shutdownTimeout`) for in-flight requests.
- The signal does not cancel request contexts. Request contexts derive from a server-lifetime context, so handlers that honour `r.Context()` finish their work normally while the server drains.
- The server releases its signal registration as soon as it observes the first signal, before it logs `server_shutdown_started` and starts the drain. A second `SIGINT` or `SIGTERM` during the drain therefore uses the default disposition and terminates the process immediately, with the signal as its exit cause, instead of waiting for the drain to finish. A single signal still drains in-flight requests and exits with status 0 once they finish.
- The server cancels the server-lifetime context only after the drain ends. A handler that outlives the 10-second drain observes a cancelled request context at that point, the shutdown is reported as failed, and the process exits with a non-zero status.

## Logging

- The server writes structured JSON logs to standard output with `log/slog`. These are service logs for log aggregation, not interactive CLI output, so they do not use ANSI color.
- Startup logs the listen address and log level. Shutdown logs the signal-driven start and the result. A listener failure is never discarded, even when it races with the shutdown signal: the process exits with a non-zero status and logs the error.
- Every HTTP request produces one `http_request` log with `method`, `path`, `status`, `duration_ms` and `bytes` fields. `status` is the first committed response status (the one the client receives), `200` for an implicit body write, and informational `1xx` responses other than `101` do not count as committed. A status code outside `100` to `999` is never committed, because `net/http` panics on it before it writes a response, so a handler that writes one is logged like a handler that panics before any status. A request whose handler panics is logged too: `status` is `500` when no response status was committed before the panic and the committed status otherwise. A flush (`http.ResponseController.Flush` or `http.Flusher`) commits `200` when no status was committed, so a handler that flushes and then panics is logged with the status the client received. A successful hijack only transfers the connection: `net/http` sends nothing for it, and the handler may write a raw response with any status or none, which the logger cannot observe. The record of a hijacked request therefore adds a boolean `hijacked` field with the value `true`, `status` is the status committed before the hijack or `200` when none was (a value that is not a client-observed status for that connection, and never `101` unless the handler wrote it before the hijack), a status written after the hijack is ignored, and a later panic is not logged as `500`. Records of requests that do not hijack have no `hijacked` field. The panic is not recovered by the logger, so `net/http` still aborts the connection. `bytes` is the number of response body bytes that `net/http` accepted from the handler. It equals the body the client receives for a response that completes. `net/http` buffers the response and discards the buffered part when the handler panics or the client disconnects, so the count can exceed what the client received; the buffered bytes of a panicking handler are counted although `net/http` discards them. It is `0` for a `HEAD` request even though the handler writes the body it would send for the equivalent `GET`; the status is the same as for that `GET`.
- A server-wide `OPTIONS *` request is logged like every other request, with `path` `*`. `net/http` would otherwise answer it before the configured handler runs, so the server construction sets `DisableGeneralOptionsHandler` and the handler answers it inside the request logger: `200` with `Content-Length: 0` and an empty body. The handler reads at most 4 KiB of request body, as the `net/http` built-in does; a larger body closes the connection after the response. Other requests with the request target `*` are rejected with `400` by the router and are logged too.
- A request whose `Expect` header is not empty and does not contain the `100-continue` token (for example `Expect: a-pony`) is rejected by `net/http` with `417 Expectation Failed` and a closed connection before it calls the handler. It is logged like every other request: one `http_request` record with its `method` and `path` (never routed, so `/missing` stays `/missing`), `status` `417` and `bytes` `0`. The `417` response, its `Connection: close` header and the connection closure are unchanged. The server serves a listener wrapped by `server.ObserveListener`, and `newServer` sets `ConnContext`, `ConnState` and `MaxHeaderBytes` for it (`MaxHeaderBytes` is `server.MaxHeaderBytes`, the `net/http` default of 1 MiB, and the observer derives its limit from the same constant):
  - The observer tees the inbound bytes of each connection, synchronously and before `net/http` reads them, into an incremental parser that holds at most one request head in memory (the head limit of `net/http`, which is 1 MiB plus 4096 bytes). It parses each head with `http.ReadRequest`, skips `Content-Length` and chunked bodies (chunk-size lines and trailers are limited to 4096 bytes each) and queues at most 1024 parsed requests that `net/http` has read but not yet dispatched. It stores only the method, the path and whether the `Expect` header is unsupported, never header values or bodies, and it starts no goroutine, so it releases everything when `net/http` closes the connection.
  - The request logger tells the observer when a handler call starts and ends, which dequeues the oldest request. The observer logs the oldest queued request when `net/http` writes a `417` response with `Connection: close` while no handler call or response of a handler is in progress and that request has an unsupported `Expect` header. The record is written once, before the response is written, as the request logger does. A request that `net/http` never answers with `417` (a client that disconnects, an earlier request that closes the connection, a shutdown, a request `net/http` rejects as malformed) produces no `http_request` record from the observer; a request that `net/http` rejects before it exists as a request produces an `http_request_rejected` record (see below).
  - If the bytes do not parse as `net/http` parses them (a malformed or oversized head, an invalid chunked body, a full queue), the observer stops parsing for that connection. A later `417` on that connection may then be missing from the logs, but a request is never logged as `417`-rejected unless `net/http` sent the `417`. The detection of `http_request_rejected` records below does not depend on the parser, so it keeps working after the parser stopped.
  - `net/http` discards up to 4 leading CR or LF bytes before a request line when the previous request on the connection was a `POST`, so the parser tracks the method of the last head that parsed and skips the same bytes in that case, wherever the reads split them. A `417` after a `POST` and a leading empty line is logged as for any other request. After another method, at the start of the connection, or with a fifth leading CR or LF byte, `net/http` answers `400` (an `http_request_rejected` record) and the parser stops.
- The one exception to the one-`http_request`-record-per-request rule is a request that `net/http` rejects before a request exists, such as a malformed request line. It has no reliable method and path, so it is not an `http_request` record. The observer logs it as one `http_request_rejected` record at `INFO` level, the level of `http_request`, so `LOG_LEVEL=warn` and `LOG_LEVEL=error` hide it like every `http_request` record. The record has these fields and no others besides the standard `time`, `level` and `msg`:
  - `reason`: one of the values below (the Go type is `server.RejectionReason`).
  - `status`: the status that `net/http` sent to the client, or `0` when it sent no response (`header_read_timeout`).
  - `duration_ms`: the time from the first byte of the rejected request (or from the connection being accepted when no byte arrived) to the rejection, in milliseconds. The request waits behind earlier pipelined requests on the connection, so it starts when the last handler finished if that was later.
  - There is no `method`, `path` or `bytes` field: none of them is known, or can be trusted, before the request parses, and the record never contains header values, the request line or the body.

  | `reason`                        | Rejection by `net/http`                                                                                                                 | `status` |
  | ------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------- | -------- |
  | `malformed_request`             | A malformed request line or header, a missing or malformed `Host` header, or a malformed partial line when `ReadHeaderTimeout` expires. | `400`    |
  | `request_header_too_large`      | A request head over `MaxHeaderBytes`.                                                                                                   | `431`    |
  | `unsupported_transfer_encoding` | A `Transfer-Encoding` other than `chunked`.                                                                                             | `501`    |
  | `unsupported_protocol_version`  | An HTTP version that `net/http` does not serve over HTTP/1.x, such as `HTTP/2.0` in the request line.                                   | `505`    |
  | `header_read_timeout`           | The client did not send a complete request head within `ReadHeaderTimeout`, and `net/http` closed the connection without a response.    | `0`      |
  | `rejected`                      | Any other status that `net/http` answers directly (not produced by a known request today).                                              | observed |
  - `net/http` answers the `400`, `431`, `501` and `505` cases itself, with one write to the raw connection of a response that has a fixed shape: `HTTP/1.1 <status> <status text>` (optionally `: <detail>`), exactly the headers `Content-Type: text/plain; charset=utf-8` and `Connection: close`, and a text body, with no `Date` and no `Content-Length`. The observer recognizes that write on the outbound bytes once the response of the last handler call is complete and logs it once, before the response is written, like the request logger. A response written by a handler always carries `Date`, so it never has this shape, and bytes of a handler's own response body are never taken for it even when they look like it: `net/http` flushes the buffered end of a handler's response after the handler returned, so the observer treats the response as in progress until `net/http` reports the connection idle through `ConnState`; a `400` written by the handler (the router rejecting the request target `*`) stays an `http_request` record. The response and the connection closure are unchanged. `net/http` closes the connection after each of these responses, so a connection logs at most one `http_request_rejected` record.
  - When `ReadHeaderTimeout` expires, `net/http` either closes the connection without any response or answers `400` and closes it, depending on the bytes after the last line break of the head it has received. It parses that partial line as if it were complete: an empty tail (a line boundary), a valid request line or a valid header line closes the connection silently, and any other tail (for example `GET /hea`, `GE`, `GET / HTTP/1.1\r`, a cut header name such as `User-Ag`, or a lone `\r` after a header) gets the `400`. During a graceful shutdown `net/http` never sends the `400`. The observer does not copy that validation. It sees the timed-out read, which `net/http` makes with a deadline set by itself, and handles it when the parser was waiting for a request head and either no request head ever parsed on the connection, or at least four bytes of the next request had arrived after an earlier request (the leading CR and LF bytes that the parser skipped after a `POST` count as received). `net/http` waits for the first four bytes of a request after a response under `IdleTimeout` and arms `ReadHeaderTimeout` only once it has them, so a stall with fewer than four bytes of the next request is the keep-alive wait and is not logged, while on a connection that has not parsed a head any partly received head or empty wait counts. With nothing after the last line break (including the skipped leading bytes and a connection that sent nothing) it logs `header_read_timeout` with `status` `0` at once. With a partial line buffered it waits for `net/http`: the `400` is logged as `malformed_request` with `status` `400` like any other `400` of this kind, and when the connection closes without a response (reported through `ConnState` as closed) it logs `header_read_timeout` with `status` `0`, the start and fields unchanged, a moment after the read. Either way it is one record, whose `duration_ms` is measured from the first byte of the rejected request. A client that connects and sends nothing for `ReadHeaderTimeout` is therefore logged too (`status` `0`, `duration_ms` about the timeout), while one that connects and closes sooner is not. These are not logged: the timeout of the keep-alive wait between requests (`IdleTimeout`), which is a normal close, including a stall after one to three bytes of the next request; the timeout of a request body (`ReadTimeout`); a read that `net/http` interrupts itself with a deadline in the past after a request; and a connection that a handler hijacked. The server sets `ConnState` to `server.ConnState` so the observer learns about a hijack and never reports a hijacked connection's own reads, writes and deadlines, and about a closed connection with a pending header timeout.
  - A request that `net/http` rejects is never logged twice: a request that reaches the handler is logged by the request logger only, a `417` rejection stays an `http_request` record, and the `http_request_rejected` record is written at most once per connection.

- `net/http` internal errors, such as the report of a recovered handler panic with its stack trace and the report of a superfluous `WriteHeader` call, are logged as `ERROR` JSON records on standard output through `http.Server.ErrorLog`. They are never written as plain text to standard error, and they are not downgraded to `INFO` or dropped when `LOG_LEVEL` is `warn` or `error`. `http.ErrAbortHandler` panics stay unlogged, as `net/http` defines. The server is constructed in one place (`newServer` in `cmd/accounts-api`) so that this setting applies to every production server.
- Authentication logs (`planned`) include the Logto user ID, the API resource and the rejection reason, according to the authentication contract.

## Build and Test

Run from the repository root:

- `pnpm --filter accounts-api dev`: run the server through the development launcher `scripts/dev.mjs` (see [Development Launcher](#development-launcher)).
- `pnpm exec turbo run build --filter=accounts-api`: build the binary into `servers/accounts-api/dist/`.
- `pnpm exec turbo run lint --filter=accounts-api`: `go vet ./...`.
- `pnpm exec turbo run test --filter=accounts-api`: `go test ./...` followed by the launcher tests, `node --test "scripts/*.test.mjs"`.

The `build` script passes `-buildvcs=false`, so the binary carries no VCS stamping (`vcs.revision`, `vcs.time` and `vcs.modified` build settings). This is a workaround with the following boundaries:

- Reason: Go's default `-buildvcs=auto` discovery recognizes only a `.git` directory. In a linked Git worktree, where `.git` is a file, it walks up to an ancestor `.git` directory. When that ancestor is not a valid repository, `go build` fails with `error obtaining VCS status`; when it is a valid but unrelated repository, the binary is stamped with the wrong revision.
- Scope: the `accounts-api` `build` script and the `go build` step of the development launcher. The `lint` and `test` scripts do not stamp binaries and are unchanged.
- Removal condition: remove the flag when the Go toolchain recognizes the linked-worktree `.git` file, or when release builds move to a normal-clone job that needs VCS stamping.

Go tasks are uncached in Turbo because the Go toolchain and module cache are not declared task inputs. Remove `servers/accounts-api/dist/` after validation.

CI runs the repository Go checks in the root Go module when Go sources or Go module files change: `gofmt` check, `go vet ./...` and `go test ./...`. See the [workflow contract](repository-workflow-contract.md). CI does not build container images or deploy the server.

### Development Launcher

`scripts/dev.mjs` replaces `go run ./cmd/accounts-api`. `go run` forwards only `SIGINT` and `SIGQUIT` to the compiled server and does not tie the server's lifetime to its own, so a `SIGTERM` sent to the `go run` process (`kill`, `timeout`, a supervisor, pnpm or Turbo) left the server listening without a graceful shutdown.

- The launcher builds the server with `go build -buildvcs=false -o <temporary directory>/accounts-api ./cmd/accounts-api`, then runs the binary as its direct child. The temporary directory is created under the operating system temporary directory and removed after the last child has exited, on every exit path.
- Child processes use explicit argument arrays and `shell: false`. The working directory is `servers/accounts-api/`; standard streams and the environment, including `ADDR` and `LOG_LEVEL`, reach the server unchanged. The server configuration, its logs and `/healthz` are unchanged, and the production binary is built by the `build` script, not by the launcher.
- The launcher forwards `SIGINT` and `SIGTERM` to the active child (the build or the server) and writes an `accounts_api_dev_signal` JSON event to standard error. It does not handle `SIGHUP`. It waits for the child to exit, so the server drains and logs `server_shutdown_started` and `server_shutdown_completed` before the launcher completes.
- On POSIX platforms the launcher starts each child (the build and the server) in its own process group (`detached: true`) and forwards each signal it receives once. A terminal Ctrl-C signals the whole foreground process group, so only the launcher receives it and the server receives exactly one `SIGINT`. Without this, the server would receive the group signal directly and again from the forward; the server releases its signal registration on the first signal, so the duplicate would terminate it during the drain. A second Ctrl-C during the drain still reaches the launcher, is forwarded and terminates the server immediately, as described in Graceful Shutdown. On Windows `detached` is not used because it opens a separate console window instead of creating a process group; the Windows behavior is unchanged and not validated.
- Orphan outcome: the server is no longer in the launcher's process group, so a `SIGKILL` of the launcher, or a `SIGHUP` to its process group (closing the terminal), stops the launcher only and leaves the server running with its listening port. Stop such a server by its process ID. `SIGHUP` forwarding is not part of the launcher.
- A signal received before the build starts (while the temporary directory is being created) stops the launcher without starting the build. A signal received during the build stops the launcher after the build exits, even when the build succeeded. The server never starts after a termination request.
- Exit status: the status of the failed build, the server's own status (non-zero for an invalid configuration or an occupied port), or `128` plus the signal number after a forwarded signal (`130` for `SIGINT`, `143` for `SIGTERM`).
- The launcher tests build and run the real server with a free loopback port. They cover a `SIGTERM` to the launcher alone, a process-group `SIGINT` with the real server, a process-group `SIGINT` delivered exactly once to a signal-counting server (the drain completes and the launcher exits `130`), a second process-group `SIGINT` during the drain that still ends the server immediately, an occupied port, an invalid `LOG_LEVEL`, a build failure, a `SIGTERM` before the build starts (a preload module delays the temporary directory creation so the window is observable; the build and the server are never started) and a `SIGTERM` during the build. They use POSIX signals and are skipped on Windows. Because the server runs in its own process group, the test cleanup kills the launcher's children by process ID before it kills the launcher's process group, and it keeps the process IDs it recorded while the launcher was alive so a server orphaned by a dead launcher is still killed. Two tests cover this cleanup for a live launcher and for a dead launcher, so a failed or timed-out launcher test leaves no server process or listening port behind.
- The `go build` step carries the same `-buildvcs=false` workaround and removal condition as the `build` script above.

## Dependencies and Integrations

- Upstream: Logto shared tenant (`planned`), payment provider (not selected), PlanetScale Postgres (`planned`).
- Downstream: [`delino-accounts-web`](apps-delino-accounts-foundation.md).
- Protocol schemas: `protos/delino/accounts/` (`planned`).

## Change Triggers

- Update [project-delino-accounts.md](project-delino-accounts.md) when the origin, component IDs, deployment target or open decisions change.
- Update [apps-delino-accounts-foundation.md](apps-delino-accounts-foundation.md) when an API capability or allowed browser origin changes.
- Add a `protos-delino-accounts-*` contract with the first Connect RPC schema.
- Update [repository-workflow-contract.md](repository-workflow-contract.md) when Go validation or deployment workflows change.
- Update `servers/AGENTS.md` only under the [instruction-update policy](README.md#instruction-update-policy).

## References

- [project-delino-accounts.md](project-delino-accounts.md)
- [repository-defaults.md](repository-defaults.md)
- [repository-auth-contract.md](repository-auth-contract.md)
- [repository-billing-contract.md](repository-billing-contract.md)
- [repository-environment-contract.md](repository-environment-contract.md)
- [repository-license-contract.md](repository-license-contract.md)
- [repository-workflow-contract.md](repository-workflow-contract.md)
- [Connect for Go](https://connectrpc.com/docs/go/getting-started)
