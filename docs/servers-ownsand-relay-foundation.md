# servers-ownsand-relay foundation contract

## Scope

This contract owns the `ownsand-relay` data plane server of [`ownsand`](project-ownsand.md) at `servers/ownsand-relay/` (`planned`). The relay server carries the traffic that would cost the most on Railway: the relay fallback of the data path (exec output and file contents) and the STUN service. It runs on a DigitalOcean Droplet, not on Railway. The [project index](project-ownsand.md#deviations-from-repository-defaults) records this deviation from the Railway default.

The relay server holds no database connection, no organization data and no sandbox content at rest. It verifies relay tickets that the [control plane](servers-ownsand-api-foundation.md) signs, forwards `DataFrame` messages between callers and device agents in memory, and answers STUN binding requests. It is licensed under `FSL-1.1-ALv2` according to the [license contract](repository-license-contract.md).

Planned layout, following the `accounts-api` server conventions:

- `servers/ownsand-relay/cmd/ownsand-relay/`: process entry point.
- `servers/ownsand-relay/internal/`: configuration, TLS, Connect handlers, ticket verification, the agent connection registry, frame forwarding and the STUN listener.
- `servers/ownsand-relay/package.json`, `turbo.json`, `.env.example` and `Dockerfile`: workspace wiring and the container image. The relay is not a Railway deployable and has no `railway.json`; the [deployment section](#deployment) owns its `deploy` field.

## Runtime and Language

- Language: Go in the root module `github.com/delinoio/mono`. Import path: `github.com/delinoio/mono/servers/ownsand-relay/...`. Logging: `log/slog`.
- Runtime: one long-running process that serves HTTPS on TCP port 443 and STUN on UDP port 3478.
- Deployment target: one DigitalOcean Droplet that runs the relay container. Production origin: `https://ownsand-relay.api.delino.io`. STUN URL: `stun:ownsand-relay.api.delino.io:3478`.
- Reason for the deviation: Railway bills network egress at $0.05 per GB, and its public networking exposes no UDP ports. DigitalOcean bills Droplet outbound transfer beyond the team's pooled Droplet transfer allowance at $0.01 per GiB, does not bill inbound transfer, and its Cloud Firewalls accept UDP rules. Each relayed byte enters the server once and leaves it once (toward the caller for exec output and file reads, toward the agent for file writes), and only the outbound leg is billed. Relay data grows with sandbox content, so it is the largest egress cost of the project. Sources: [Railway pricing plans](https://docs.railway.com/pricing/plans), [DigitalOcean bandwidth billing](https://docs.digitalocean.com/platform/billing/bandwidth/) and [DigitalOcean firewall rules](https://docs.digitalocean.com/products/networking/firewalls/how-to/configure-rules/).
- TLS: the process terminates TLS itself and serves HTTP/2 and HTTP/1.1. The DNS record of `ownsand-relay.api.delino.io` points directly at the Droplet and must not go through an HTTP proxy (for example, a proxied Cloudflare record), because the STUN service needs the Droplet address and the agent connection needs HTTP/2 from end to end. Record the certificate source and its renewal with the implementation.
- Railway edge limits do not apply to the relay server. The relay server sets its own limits in [Interfaces and Contracts](#interfaces-and-contracts).
- Instances: the first version runs exactly one relay server. Each relay ticket names the relay origin, so a later change can add relay servers (for example, per region) without a protocol change. Record the selection rule in this contract and in the [control plane contract](servers-ownsand-api-foundation.md#data-path-signaling-and-relay) before a second relay server runs.
- Firewall: a DigitalOcean Cloud Firewall allows inbound TCP 443, UDP 3478 and the port that the certificate source needs, and allows SSH only from operator addresses. Record the rules with the deployment.
- Development port (`planned`): `127.0.0.1:8301` for HTTPS and `127.0.0.1:3478` for STUN. Add them to the [environment contract](repository-environment-contract.md) in the change that adds the development launcher.

## Users and Operators

- Callers (the [SDKs](packages-ownsand-sdk-foundation.md) and the [CLI](cmds-ownsand-cli-foundation.md)) that hold a caller relay ticket from `DataPathService.AuthorizeRelay`.
- [Device agents](cmds-ownsand-agent-foundation.md) that hold an agent relay ticket from a relay grant.
- Callers and device agents that send STUN binding requests.
- Operators who deploy and monitor the Droplet.

## Interfaces and Contracts

- Business APIs: `RelayService` and `RelayAgentService` of the [protos contract](protos-ownsand-api-contract.md#relay-services).
- Operational endpoint: `GET /healthz` without authentication returns `200` with `{"status":"ok"}` while the process can serve HTTP requests.
- Browser access: none in this version. The relay server sends no CORS headers.

### Relay tickets

- The control plane signs every relay ticket with an Ed25519 key. The relay server holds only the public keys, so it cannot issue a ticket. The ticket format and fields are owned by the [protos contract](protos-ownsand-api-contract.md#relay-tickets).
- The relay server accepts a ticket only when its signature verifies with a configured public key that matches its key ID, its `relay_origin` equals the relay server's own origin, its expiry has not passed, with a fixed tolerance of 30 seconds for clock skew, and its kind matches the RPC: `CALLER` tickets on `RelayService`, `AGENT` tickets on `RelayAgentService`. It rejects any other ticket with `unauthenticated` and the same message. It never sends a ticket to the control plane.
- Key rotation: the relay server accepts every configured public key. To rotate, an operator adds the new public key to the relay server first, then switches the control plane to the new private key, and removes the old public key only after the latest `expires_at` that it signed plus the 30-second clock-skew tolerance has passed.
- A caller ticket authorizes `RelayService` calls only for its `sandbox_id` and its operation (`exec_id`, `read_id` or `write_id`), on its `device_id`, until its expiry. The relay server checks the request's `organization_id`, `sandbox_id`, operation kind and operation ID against the ticket and rejects a mismatch with `permission_denied`; the agent checks the sandbox, operation kind and operation ID again against its grant; it enforces the organization boundary through the sandbox of the grant, which the control plane issued for that organization. An RPC must start before the ticket expires (plus the 30-second clock-skew tolerance). A stream that started before the expiry continues until it ends according to [Relay streams](#relay-streams).
- The relay server does not check revocation itself. The device agent is the enforcement point: it admits a relay stream only for a ticket ID of a live relay grant that the control plane sent on the agent channel (grant expiry blocks only new requests), and it ends the admitted streams of a grant when the control plane revokes it, when that agent channel closes or at the grant's stream deadline, according to the [agent contract](cmds-ownsand-agent-foundation.md#data-path). A compromised relay server therefore cannot start a request on a sandbox without a live grant.

### Agent connections

- A device agent opens `RelayAgentService.Connect`, a bidirectional stream over HTTP/2, with the `AGENT` ticket of a relay grant in the `Authorization: Bearer` header, and sends its request headers with an initial keepalive frame at once. The relay server checks the `AGENT` ticket only when `Connect` opens, and registers the connection for the ticket's `device_id`. The connection continues after that ticket expires, until the keepalive timeout, a replacement or a close by the agent. When a second connection for the same device opens, the relay server ends the RPCs on the older connection with `unavailable` and closes it.
- Device network requirement: the agent connection needs outbound TCP 443 with HTTP/2 from end to end and no TLS interception. A WebSocket transport for `Connect`, for devices behind proxies that allow only HTTP/1.1, is an open decision; record it here before such devices are supported.
- Each side sends a keepalive frame after 30 seconds without an outbound frame, so one-way traffic never lets the other side's 90-second receive timeout expire. The relay server closes a connection that receives nothing for 90 seconds, and the agent treats 90 seconds without a received frame as a lost connection and reconnects.
- When an agent connection closes, the relay server ends every RPC routed on it with `unavailable`.

### Relay streams

- For each `RelayService` call, as soon as it has verified the ticket in the request headers (before it receives a unary request body), the relay server allocates a `stream_id` that is unique among the open RPCs on that agent connection, sends a stream-open frame with the ticket ID and the `sandbox_id`, translates the request into `DataFrame` messages and translates the agent's frames back into responses. The [protos contract](protos-ownsand-api-contract.md#data-path-framing) owns the frames.
- When no agent connection for the ticket's device exists, the relay server waits up to 10 seconds for one, and then fails the RPC with `unavailable`.
- When the agent ends a stream with a session-end frame (including `GRANT_INVALID` for a stream open that names no live grant), the relay server ends the RPC with `unavailable` and the `SessionEndReason` in the error detail. The caller then calls `AuthorizeRelay` again, which returns the authorization error when the ticket was revoked.
- When a relay RPC ends on the caller side or at the stream limit, the relay server sends a relay stream close frame to the agent, according to the [protos contract](protos-ownsand-api-contract.md#data-path-framing). It never reuses a `stream_id` on one agent connection.
- A server stream sends a `keepalive` event at least every 60 seconds while it has nothing else to send, also before the agent's `accepted` event, so that caller-side proxies and NAT devices keep the connection. The relay server ends each stream before it has run for 14 minutes with an OK status and no final event (a stream rotation), so that every long operation is authorized again within a bounded time; the caller continues with the resume rules of the [protos contract](protos-ownsand-api-contract.md#exec).
- A `WriteFile` call must finish within 5 minutes, including the upload of its request body and the agent's result, and the relay server fails a slower call with `deadline_exceeded`. The SDKs retry the chunk with the same `write_id`.
- Forwarding uses one bounded in-memory buffer for each stream. Toward the caller, the relay server grants the agent a credit window per stream that never exceeds the free space in that stream's buffer, and sends a relay window update as the caller consumes data, according to the [protos contract](protos-ownsand-api-contract.md#data-path-framing). A slow caller therefore slows only its own stream: the relay server never stops reading the shared agent connection, never drops a frame and never exceeds a buffer. An agent that sends beyond its window breaks the protocol, and the relay server closes that agent connection. Toward the agent, the relay server forwards request payload frames only within the credit window that the agent grants for each stream, and applies back-pressure to that caller's request body only. The relay server never writes frames to disk, to logs or to object storage. Record the per-connection buffer limit and the concurrency limits per agent connection and per ticket here before the relay implementation.
- A relay server restart or deployment ends every open RPC and agent connection. Callers continue with the resume rules, and agents reconnect while they hold a live grant.

### STUN

- The relay server answers STUN binding requests ([RFC 8489](https://www.rfc-editor.org/rfc/rfc8489.html)) on UDP port 3478 without authentication. It does not offer TURN and does not relay media or data over UDP.
- The control plane returns the STUN URL from `DataPathService.GetIceServers` and in the agent configuration message. The URL is control plane configuration, so a change of the STUN address needs no client release.
- Rate-limit STUN responses per source address. A binding response can be larger than its request ([RFC 8489 section 16.1.2](https://www.rfc-editor.org/rfc/rfc8489.html#section-16.1.2)), so the limit bounds the reflected traffic toward a spoofed address but does not remove it. Record the request and response byte budget per source address with the implementation.
- The STUN server implementation (an in-process library, for example the pion STUN package, so that one process serves HTTPS and STUN) is a new dependency. Record it here with its dependency approval.

## Storage

The relay server stores nothing persistently. It keeps agent connections, open RPCs and frame buffers in process memory only.

## Security

- Accept only the relay tickets in [Relay tickets](#relay-tickets). Never accept Logto access tokens, API keys or device session credentials; callers never send them to the relay origin.
- The relay server is content-blind for storage: it forwards sandbox content in memory only. It terminates TLS, so it can read and change relayed content in transit; the request digest in each grant stops it from changing the command or the guest path of an operation, but the project trusts it for the confidentiality and integrity of the relayed content, as described in the [control plane contract](servers-ownsand-api-foundation.md#security). The Droplet hardening below follows from that trust.
- Configuration (the relay origin, the relay ticket public keys with their key IDs, the TLS configuration and the STUN rate limit) comes from the Droplet's environment according to the [environment contract](repository-environment-contract.md). The relay server needs no secret except its TLS private key. `.env.example` lists the names with placeholders only.
- Rate-limit ticket verification failures per client address. The relay server terminates TCP itself, so the client address is the connection's peer address.
- The Droplet runs only the relay container and the components that the deployment needs. SSH access is limited to operator keys and addresses, and the TLS private key is readable only by the relay process. Record the operating system update policy and the access list with the deployment.

## Logging

- Structured JSON logs on standard output with `log/slog`. Record where the Droplet sends them with the deployment.
- One record per RPC with the procedure, ticket ID, organization ID, sandbox ID, device ID, Connect code, frame counts, byte counts and duration.
- Agent connection events: device ID, connect, disconnect, replacement and the number of RPCs that each disconnect ended.
- STUN: aggregate request and rate-limit counts, not one record per request.
- Never log tickets, signed ticket bodies, TLS keys, exec commands, environment values, exec output or file contents.

## Build and Test

Record the package scripts with the implementation, following `accounts-api`:

- `pnpm exec turbo run build --filter=ownsand-relay`, `lint` (`go vet ./...`) and `test` (`go test ./...`).
- From the repository root: `gofmt` check, `go vet ./...` and `go test ./...`, according to `servers/AGENTS.md`.
- Required tests:
  - Tickets: bad signature, unknown key ID, acceptance of every configured public key during key rotation, expired ticket and the 30-second clock skew on `expires_at`, wrong relay origin, wrong kind, mismatched organization, sandbox or operation ID.
  - Agent connections: replacement, the 10-second wait for an agent connection, the 90-second idle close on the relay server and the agent's 90-second idle reconnect, an `AGENT` connection that outlives its ticket, a cold agent connection that registers within the 10-second caller wait, retention of a rotated public key through the 30-second skew tolerance, and closing an agent connection that sends beyond its credit window.
  - Streams and flow control: stream keepalive, stream rotation with an OK status before 14 minutes, relay stream close frames and `stream_id` non-reuse, per-stream credit windows with one slow and one fast caller on the same device, the 5-minute `WriteFile` call deadline including the agent's result, session-end frames, and forwarding without persistence.
  - STUN binding responses and rate limiting, `GET /healthz`, and log redaction.

## Deployment

- The [deployment workflow](repository-workflow-contract.md#deployment-workflow) deploys only Railway servers and Cloudflare Pages sites today. Record the Droplet provisioning, the deployment job, its credentials, its rollback procedure and the `deploy` target of the relay package in the workflow contract before the first deployment of the relay server. Until then, the relay package has no `deploy` field and the relay server is not deployed.
- The deployment updates the relay server before the control plane when a change needs both, so that the relay server accepts a new public key or frame kind before the control plane uses it.

## Dependencies and Integrations

- Upstream: the [control plane](servers-ownsand-api-foundation.md), which signs relay tickets and sends relay grants to agents. The relay server makes no request to the control plane.
- Peers: [SDKs](packages-ownsand-sdk-foundation.md), the [CLI](cmds-ownsand-cli-foundation.md) and [device agents](cmds-ownsand-agent-foundation.md).
- Protocol schemas: [protos contract](protos-ownsand-api-contract.md).
- Platform: DigitalOcean Droplets and Cloud Firewalls.

## Change Triggers

- Update [project-ownsand.md](project-ownsand.md) when the origin, the STUN URL, the deployment target or the number of relay servers changes.
- Update the [protos contract](protos-ownsand-api-contract.md) with every `RelayService`, `RelayAgentService`, ticket or `DataFrame` change.
- Update the [control plane contract](servers-ownsand-api-foundation.md) and the [agent contract](cmds-ownsand-agent-foundation.md) when ticket, grant or revocation rules change.
- Update [repository-workflow-contract.md](repository-workflow-contract.md) when the relay server joins the deployment workflow.
- Update `servers/AGENTS.md` only under the [instruction-update policy](README.md#instruction-update-policy).

## References

- [project-ownsand.md](project-ownsand.md)
- [servers-ownsand-api-foundation.md](servers-ownsand-api-foundation.md)
- [servers-accounts-api-foundation.md](servers-accounts-api-foundation.md)
- [repository-defaults.md](repository-defaults.md)
- [repository-environment-contract.md](repository-environment-contract.md)
- [repository-license-contract.md](repository-license-contract.md)
- [repository-workflow-contract.md](repository-workflow-contract.md)
- [DigitalOcean bandwidth billing](https://docs.digitalocean.com/platform/billing/bandwidth/)
- [Railway pricing plans](https://docs.railway.com/pricing/plans)
