# cmds-ownsand-agent foundation contract

## Scope

This contract owns two components of [`ownsand`](project-ownsand.md):

- `cmds/ownsand-agent/` (`planned`): the device agent `ownsand-agent`. It runs as a system service on each enrolled Linux, macOS or Windows host, keeps an outbound channel to the control plane, runs sandboxes in VMs, enforces the network policy and TTLs, takes and moves snapshots, serves the data path and updates itself.
- `cmds/ownsand-guest/` (`planned`): the guest helper `ownsand-guest`. It runs inside every sandbox guest and serves exec and file requests from the agent.

Both components use Apache-2.0 and must not import code from `servers/`.

## Runtime and Language

- Language: Go in the root module `github.com/delinoio/mono`. Logging: `log/slog`.
- The agent runs as a system service: a systemd unit on Linux, a launchd daemon on macOS and a Windows service on Windows. It needs the privileges of its hypervisor and network setup; each platform section below names them.
- Platform code is separated by Go build constraints. Code for one host OS must build for that OS only, and the shared code must build for all three.
- The macOS agent binary must be code-signed with the `com.apple.security.virtualization` entitlement, which Virtualization.framework requires. Record the signing identity and the notarization process with the release process.
- Hypervisor bindings (Virtualization.framework from Go, Hyper-V Host Compute Service from Go) and the WebRTC library (pion) are new dependencies. Each needs owner approval in the change that adds it.

## Users and Operators

- Device owners and organization admins, who install, enroll, update and remove the agent.
- The [control plane](servers-ownsand-api-foundation.md), which sends commands on the agent channel.
- Callers (SDK and CLI), which connect peer-to-peer to the agent's data path.

## Interfaces and Contracts

### Commands

| Command                     | Purpose                                                                                     |
| --------------------------- | ------------------------------------------------------------------------------------------- |
| `ownsand-agent enroll`      | Enroll the device with a one-time enrollment token and create the device key pair.          |
| `ownsand-agent run`         | Run the service. The service manager starts this command.                                   |
| `ownsand-agent status`      | Print the enrollment, connection, capability, sandbox and update state.                     |
| `ownsand-agent image build` | Build the macOS or Windows base image locally (see [Images](#images)).                      |
| `ownsand-agent reset`       | Delete all local sandboxes, snapshots, images and the device key without the control plane. |

- `enroll` reads the enrollment token from standard input or from a file named by `--token-file`. It never accepts the token as a command-line argument, because other local users can read process arguments.
- CLI output uses ANSI color by default. `--no-color` and the `NO_COLOR` environment variable turn color off. The service writes JSON logs without color.

### Capabilities

The agent reports its capabilities after each authentication, again when its built images or hypervisor checks change, and its free disk at least every 60 seconds and whenever it changes by more than 1 GiB: host OS, architecture, agent version, logical CPU count, memory, free disk in the state directory, available guest OSes, built base images, whether macOS guests are supported (Apple silicon only), the supported macOS hardware models, and the hypervisor checks below. The control plane uses them for [scheduling](servers-ownsand-api-foundation.md#scheduler).

### Isolation

Every sandbox is a VM. The agent never runs sandbox code in a host process or container.

| Host OS | Hypervisor                               | Guests         | Host requirements                                                                                  |
| ------- | ---------------------------------------- | -------------- | -------------------------------------------------------------------------------------------------- |
| Linux   | microVM (Firecracker class) on KVM       | Linux          | Access to `/dev/kvm`; `amd64` or `arm64`.                                                          |
| macOS   | Virtualization.framework                 | Linux, macOS   | macOS guests only on Apple silicon; at most two macOS guests at a time. Linux guests do not count. |
| Windows | Hyper-V through the Host Compute Service | Linux, Windows | Windows 11 Pro, Enterprise or Education, or Windows Server, with Hyper-V enabled.                  |

- The guest architecture is the host architecture.
- Linux: the microVM implementation is an open decision (Firecracker class). Record the selected implementation, its version pin and how the agent runs it (for example, a jailer with one unprivileged user per sandbox) before the Linux runtime implementation.
- macOS: a Virtualization.framework VM runs in the process that created it. Restarting the agent stops every running and paused VM on every host OS, because every VM is bound to the agent process (see [TTL](#ttl)); on macOS the VMs also run inside the agent process. This is one reason for the update rule in [Agent updates](#agent-updates).
- Each sandbox gets the vCPU count, memory and disk of its create request. The agent rejects a create command that exceeds the device limits that the control plane sent, or that would start a third macOS guest, as a second check after the scheduler.

### Guest helper

- `ownsand-guest` starts with the guest and listens on the hypervisor socket of the platform: vsock on Firecracker-class microVMs, a Virtualization.framework virtio socket device on macOS hosts and a Hyper-V socket on Windows hosts. It does not listen on any guest network interface.
- The agent sends `DataFrame` exec and file requests to the guest helper and forwards its responses to the data path. The guest helper serves the `GuestService` of the [protos contract](protos-ownsand-api-contract.md#guest-channel).
- The guest helper is part of every base image. For locally built macOS and Windows images, the image build installs it.

### Network policy

- The default mode `OUTBOUND` gives the guest outbound internet access through NAT on the host.
- In `OUTBOUND` mode, the agent drops every guest packet to these destinations: the host's own addresses on all interfaces; `0.0.0.0/8`, `10.0.0.0/8`, `100.64.0.0/10`, `127.0.0.0/8`, `169.254.0.0/16`, `172.16.0.0/12`, `192.168.0.0/16`, `224.0.0.0/4` and `240.0.0.0/4`; `::1/128`, `fc00::/7`, `fe80::/10` and `ff00::/8`; `64:ff9b:1::/48`; the IPv4-mapped (`::ffff:0:0/96`) and NAT64 well-known prefix (`64:ff9b::/96`) forms of the IPv4 ranges; and, from the host routing table for IPv4 and IPv6, every on-link prefix of every host interface and every prefix that the host reaches through a route other than the default route. A complementary pair of `/1` routes through the same interface (for example, the `0.0.0.0/1` and `128.0.0.0/1` pair that a VPN installs to replace the default route, or `::/1` and `8000::/1`) counts as a default route, so a full-tunnel VPN does not block the internet. A single `/1` route without its complement through the same interface is an ordinary route and its prefix is blocked. These prefixes cover LANs and routed internal networks that use globally routed addresses, such as most IPv6 LANs. An internal network that uses global addresses and that the host reaches only through its default route looks like the internet to the host, so the agent cannot detect it; a configurable list of extra blocked prefixes is not part of this version. The agent updates the rules when interfaces, addresses or routes change. Inbound connections to the guest are not allowed.
- DNS: the agent serves DNS to the guest at the guest gateway address and resolves the queries through the host resolver. The gateway accepts only DNS (UDP and TCP port 53) from the guest. On the guest link only, the agent also allows DHCPv4 (UDP ports 67 and 68, including the broadcast to `255.255.255.255`), DHCPv6 and the ICMPv6 router and neighbor discovery messages between the guest and its gateway, so guests that configure their network automatically get an address. These are the only exceptions to the host-address and blocked-range rules. A name that resolves to a blocked address stays unreachable, because the packet filter applies after resolution.
- `NONE` mode attaches no network device to the guest. The guest helper still works through the hypervisor socket.
- Enforcement per host:
  - Linux: a tap device per sandbox with nftables rules in a table that the agent owns. The agent needs `CAP_NET_ADMIN`.
  - macOS: the built-in Virtualization.framework NAT attachment cannot filter LAN destinations. The agent attaches the guest network through a file-handle attachment to a userspace network stack in the agent that applies the policy. The network stack library is an open decision and a new dependency.
  - Windows: open decision between Host Networking Service ACL policies on the sandbox endpoint and a Windows Filtering Platform filter. Record the mechanism before the Windows runtime implementation.
- Network policy tests must show, on each host OS, that the guest reaches the internet in `OUTBOUND` mode and cannot reach the host, the LAN, the link-local range or a network behind a non-default route, also on a LAN with global IPv6 addresses and on a host with a full-tunnel VPN and a separate internal route, and that a `NONE` guest has no network.

### TTL

The agent stores the idle and maximum deadlines of each sandbox in its state directory and enforces them as the [control plane contract](servers-ownsand-api-foundation.md#ttl) defines, also while it is disconnected. Every VM is bound to the agent process, so that `max_ttl` holds even when the agent stops: when the agent process ends, its VMs stop (on Linux through a cgroup that the service manager kills when the agent exits (systemd `KillMode=control-group`) and a parent-death signal of each VM process, which the agent sets from one dedicated launcher thread that lives as long as the agent process (Linux sends the parent-death signal when the creating thread exits, not only the process), on Windows through the Host Compute Service setting `ShouldTerminateOnLastHandleClosed` on each compute system, with the agent holding the only handle, so that HCS ends the VM when the agent process ends, and on macOS because the VMs run inside the agent process). After a restart, it enforces deadlines that passed while it was stopped at once, also for a VM that survived because this binding failed. When a deadline passed and the VM is also gone (for example, after an agent restart), the TTL end reason wins over `LOST_ON_DEVICE`.

### Images

- `LINUX` base image: built and signed by the OwnSand release process and verified with the same built-in keys as agent releases. Record the distribution, the kernel source and the image format with the Linux runtime implementation.
- `MACOS` base image: built on the device from an Apple IPSW. The agent uses the latest supported restore image or an IPSW file that the admin names. The installation and the first-boot setup that installs the guest helper are open technical work.
- `WINDOWS` base image: built on the device from an ISO file that an admin supplies with `ownsand-agent image build --iso <path>`. The agent installs Windows unattended and installs the guest helper. Delino does not distribute the ISO or a product key.
- The organization is responsible for the license terms of its guest operating systems.
- Templates are snapshots with a name. A sandbox created from a template uses the snapshot's disk.

### Pause, resume and snapshots

- Pause stops the VM's vCPUs and keeps its memory. Resume continues it. Paused VMs also end when the agent process ends, on every host OS.
- A snapshot captures the guest disk. Where the hypervisor supports it, the snapshot also captures the memory state; record per hypervisor which content a snapshot holds before its implementation. A sandbox created from a snapshot is a new sandbox with a new ID.
- Snapshots stay in the agent state directory by default.
- A macOS snapshot also holds the VM's auxiliary storage, its hardware model and its machine identifier, which Virtualization.framework needs to boot the disk. A macOS snapshot can restore only on a Mac that reports support for its hardware model. The agent reports its supported macOS hardware models as a capability and checks support again before an import. The agent never runs two VMs with the same macOS machine identifier at the same time. Whether a restored macOS disk boots with a new machine identifier must be verified in the change that implements the macOS runtime. Until that is verified, the scheduler reserves machine identifiers according to the [control plane scheduler](servers-ownsand-api-foundation.md#scheduler), and the agent rejects a create that would reuse a running identifier.
- Export: the agent uploads the snapshot to R2 with the signed URLs that the control plane issues, in parts, and reports the SHA-256 of each part. Import: the agent downloads with signed URLs, checks the hashes that the import command carries and only then marks the snapshot ready. An exported snapshot can run only on a device with the same host OS and architecture.

### Data path

- The agent accepts a WebRTC data channel only for an `OpenSession` offer that it received from the control plane on the agent channel, and only from the DTLS fingerprint in that offer.
- The agent answers with its ICE candidates, including STUN-derived server-reflexive candidates. When ICE fails, the caller uses the [relay server](servers-ownsand-relay-foundation.md), and the agent serves the same `DataFrame` requests from its relay connection.
- Relay grants: the agent accepts a relay grant only on the agent channel, acknowledges it and binds it to that channel. While it holds at least one live grant (unexpired, unrevoked and bound to the current agent channel, according to the [control plane contract](servers-ownsand-api-foundation.md#data-path-signaling-and-relay)) for a relay origin, it keeps one `RelayAgentService.Connect` connection to that origin. It picks the `AGENT` ticket of its newest live grant only when it dials; a new grant reuses a healthy connection and never replaces it, so streams of older grants continue. When the connection is lost or replaced, the agent ends every relay stream of that connection and releases their output subscriptions, as for a relay stream close frame; exec and file operation state keeps its own retention rules. It reconnects with back-off of at most 5 seconds between attempts, below the relay server's 10-second wait for an agent connection, and only while a live grant exists, and closes the connection 60 seconds after it last had no live grant and no open relay stream, whether the last grant was released, revoked, replaced, expired or lost with the agent channel. The relay connection is an outbound HTTPS connection; it opens no listening port.
- The agent sends payload frames toward the caller only within the relay window that the relay server grants for each stream, according to the [protos contract](protos-ownsand-api-contract.md#data-path-framing).
- The agent serves a relay stream only for a relay stream open frame whose ticket ID names a live grant for the frame's `sandbox_id`, according to the [protos contract](protos-ownsand-api-contract.md#data-path-framing). A grant that expires stops new relay streams but does not end open ones; the agent ends the open ones at the grant's stream deadline with `EXPIRED`. A relay stream open for no live grant gets a session-end frame with `GRANT_INVALID`. The agent checks the first request frame of each relay stream against the grant's operation kind, ID and request digest, rejects it when the grant expired before it arrived, and applies the sandbox state rule of that request before it dispatches it, and releases a stream's output subscription when it receives a relay stream close frame, without stopping the exec.
- The agent enforces the session end rules of the [control plane contract](servers-ownsand-api-foundation.md#data-path-signaling-and-relay) on its data channels and on the relay streams of each grant: idle time and absolute expiry for sessions, and revocation messages and loss of the agent channel for both. It sends a session-end frame on each affected relay stream before it ends it.
- The agent runs each exec independently of the data path that started it, and keeps its recent output and exit result for `AttachExec` according to the [protos contract](protos-ownsand-api-contract.md#exec).
- The agent never sends sandbox content anywhere except to the data channel of the session or to its relay connection, for a stream that it admitted under a live grant and that has not ended, been revoked or reached its stream deadline.
- The agent records the request digest of each exec, read and write when it first accepts the operation on either transport, checks continuations against it, and releases a grant on the agent channel when the grant has no open stream and its operation finished, according to the [protos contract](protos-ownsand-api-contract.md#relay-tickets) and the [control plane contract](servers-ownsand-api-foundation.md#data-path-signaling-and-relay).
- When the agent accepts a server-streaming relay request (`Exec`, `AttachExec` or `ReadFile`), it sends a keepalive event for that stream at once, so the caller sees that the request was accepted before any output arrives. `WriteFile` is unary and has no acceptance event.
- The agent acknowledges each relay grant with a result: accepted, or rejected because of its per-device grant limit. It grants credit windows for request payload frames on each relay stream and buffers at most that window.

### Device removal

When the agent receives a clean-up command or a removal notice from `Authenticate`, it stops and deletes every local sandbox, snapshot and locally built image. It records in its state directory that it received an authenticated removal notice and finished the clean-up. Then it calls `ConfirmRemoval` with a nonce from `CreateChallenge` signed by its device key, retrying with back-off until the call succeeds, or until `ConfirmRemoval` reports the device as unknown: in this recorded state, an unknown device means that an earlier confirmation succeeded and its response was lost. Only then does it delete the device key and exit enrolled state. The control plane deletes the removal tombstone when `ConfirmRemoval` succeeds. `ownsand-agent reset` does the same without the control plane.

When `Authenticate` reports that the device is unknown and the agent has no record of a removal notice, the agent keeps its local data, logs the error and retries with back-off, because the control plane keeps a tombstone for every removed device for up to 1 year. An operator runs `ownsand-agent reset` to delete the data.

### Agent updates

- The control plane sends the target version on the agent channel according to the organization's [agent version policy](servers-ownsand-api-foundation.md#agent-version-policy).
- The agent downloads a signed release manifest and the release artifact for its platform. The manifest names the version, the platform, the SHA-256 of the artifact and the versions that are revoked. The agent verifies the manifest signature with a public key that is built into the agent, and verifies the artifact hash, before it installs anything. It rejects a release with a bad signature, a hash mismatch or a revoked version, and reports `FAILED`.
- The agent keeps a list of built-in public keys so that a release signed with the current key can introduce the next key.
- The agent installs an update only when no sandbox is `creating`, `running` or `paused` and no snapshot export or import runs. Starting the installation and accepting a create command are mutually exclusive on the agent: once the agent reports `INSTALLING`, it rejects create commands with `unavailable`, and it does not start the installation while a create is in progress. Until then, it reports `PENDING_IDLE` and keeps serving its sandboxes. It never stops a sandbox to install an update.
- When the signed manifest revokes the running version, the agent rejects new create commands and reports the update state `REVOKED_PENDING_IDLE`, so the scheduler stops placing sandboxes on it. Running sandboxes end at their `max_ttl` at the latest, which every sandbox must have, and the agent then installs the update. Revocation overrides the organization's `HOLD` or `PIN` policy: the control plane then sends the default target version, according to the [control plane contract](servers-ownsand-api-foundation.md#agent-version-policy).
- A version lower than the running version is installed only when the organization policy pins it, or when the control plane marks the target as a revocation recovery because the running or pinned version is revoked.
- The release signing tool and the key custody process are open decisions. Record them here before the first release.

## Storage

- State directory, readable only by the agent's service account: `/var/lib/ownsand-agent` on Linux, `/Library/Application Support/ownsand-agent` on macOS and `%ProgramData%\ownsand-agent` on Windows.
- Contents: an identity file with the device ID and the Ed25519 private key, kept apart from the other records so that the loss of sandbox records does not lose the identity; the control plane origin, the sandbox and snapshot records with their TTL deadlines, VM disks, memory state, snapshots, base images and the pending update.
- The agent never writes enrollment tokens, session credentials, relay tickets, signed URLs, exec content or file content to its logs.

## Security

- The device private key never leaves the device. Removal and `reset` delete it.
- Sandbox content stays in the VM, the state directory, the data channel, the relay connection or an explicit R2 export.
- The agent validates every guest path request inside the guest helper, so a guest path never touches the host file system.
- The agent's own connections go only to the control plane origin, the relay origin of a live grant, the STUN service, R2 signed URLs, release downloads and, during `ownsand-agent image build` without an operator-supplied IPSW, Apple's restore image lookup and download. Guest traffic that the [network policy](#network-policy) allows and the agent's DNS forwarding for guests are guest traffic, not agent connections; on macOS the agent's userspace network stack opens them for the guest. The agent has one more exception: for a session that `OpenSession` authorized, its ICE agent binds ephemeral UDP sockets, sends connectivity checks to the caller's candidates and accepts the caller's checks and traffic. These sockets exist only for the session and accept only the DTLS fingerprint of the offer. The agent opens no other listening port on the host network.

## Logging

- Structured JSON logs with `log/slog` to the platform service log (journald, the unified log or the Windows event log, or a log file in the state directory; record the choice with the implementation).
- Events: enrollment result, authentication result, channel connect and disconnect, capability report, command ID and result, sandbox state change, TTL expiry, network setup result, snapshot export and import progress, data path transport selection, and update verification and installation.
- Never log enrollment tokens, session credentials, relay tickets, private keys, signed URLs, SDP bodies, exec commands, environment values, exec output or file contents.

## Build and Test

- From the repository root: `gofmt` check, `go vet ./...` and `go test ./...`, according to `cmds/AGENTS.md`.
- CI runs on Linux only and excludes macOS runners from pull requests and main pushes, according to the [workflow contract](repository-workflow-contract.md). Cross-compile the macOS and Windows code paths with `GOOS` on Linux so that CI checks that they build; behavior on macOS and Windows needs device acceptance.
- Unit tests: TTL clocks, update verification (bad signature, hash mismatch, revoked version, update while sandboxes run, pinned version), state report reconciliation (including `LOST_ON_DEVICE`), network policy rule generation (including on-link prefix changes), session end rules, relay grant binding, expiry, stream deadline and revocation, relay stream close, relay connection loss, connection reuse when a new grant arrives, request digest and expiry checks on the first request frame, relay windows, the per-device grant limit, per-request state checks on relay streams, revocation targets, relay connection lifetime, exec output retention and token handling.
- Device acceptance on real Linux, macOS and Windows hosts: the full lifecycle, the network policy tests, the relay fallback, disconnect and reconnect, snapshot export and restore on a second device, removal clean-up and signed updates. Record the results in pull requests and Linear issues, separate from CI.

## Dependencies and Integrations

- Upstream: the [control plane](servers-ownsand-api-foundation.md), the [relay server](servers-ownsand-relay-foundation.md) (relay connections and STUN), Cloudflare R2 signed URLs, the release manifest host.
- Platform: KVM and the microVM runtime, Virtualization.framework, Hyper-V Host Compute Service.
- Protocol schemas: [protos contract](protos-ownsand-api-contract.md).

## Change Triggers

- Update the [control plane contract](servers-ownsand-api-foundation.md) when the agent channel, commands, TTL rules or clean-up rules change.
- Update the [protos contract](protos-ownsand-api-contract.md) when `AgentService`, `GuestService` or `DataFrame` changes.
- Update [project-ownsand.md](project-ownsand.md) when host requirements, the open decisions or the licensing boundary change.
- Update `cmds/AGENTS.md` only under the [instruction-update policy](README.md#instruction-update-policy).

## References

- [project-ownsand.md](project-ownsand.md)
- [repository-defaults.md](repository-defaults.md)
- [repository-workflow-contract.md](repository-workflow-contract.md)
- [repository-dependency-security-contract.md](repository-dependency-security-contract.md)
- [Virtualization.framework](https://developer.apple.com/documentation/virtualization)
- [Firecracker](https://firecracker-microvm.github.io/)
- [Hyper-V Host Compute Service](https://learn.microsoft.com/en-us/virtualization/api/hcs/overview)
