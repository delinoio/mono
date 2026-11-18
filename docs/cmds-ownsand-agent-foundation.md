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

The agent reports its capabilities after each authentication: host OS, architecture, agent version, logical CPU count, memory, free disk in the state directory, available guest OSes, built base images, and the hypervisor checks below. The control plane uses them for [scheduling](servers-ownsand-api-foundation.md#scheduler).

### Isolation

Every sandbox is a VM. The agent never runs sandbox code in a host process or container.

| Host OS | Hypervisor                               | Guests         | Host requirements                                                                                  |
| ------- | ---------------------------------------- | -------------- | -------------------------------------------------------------------------------------------------- |
| Linux   | microVM (Firecracker class) on KVM       | Linux          | Access to `/dev/kvm`; `amd64` or `arm64`.                                                          |
| macOS   | Virtualization.framework                 | Linux, macOS   | macOS guests only on Apple silicon; at most two macOS guests at a time. Linux guests do not count. |
| Windows | Hyper-V through the Host Compute Service | Linux, Windows | Windows 11 Pro, Enterprise or Education, or Windows Server, with Hyper-V enabled.                  |

- The guest architecture is the host architecture.
- Linux: the microVM implementation is an open decision (Firecracker class). Record the selected implementation, its version pin and how the agent runs it (for example, a jailer with one unprivileged user per sandbox) before the Linux runtime implementation.
- macOS: a Virtualization.framework VM runs in the process that created it. Restarting the agent stops every running and paused VM on macOS. This is one reason for the update rule in [Agent updates](#agent-updates).
- Each sandbox gets the vCPU count, memory and disk of its create request. The agent rejects a create command that exceeds the device limits that the control plane sent, as a second check after the scheduler.

### Guest helper

- `ownsand-guest` starts with the guest and listens on the hypervisor socket of the platform: vsock on Firecracker-class microVMs, a Virtualization.framework virtio socket device on macOS hosts and a Hyper-V socket on Windows hosts. It does not listen on any guest network interface.
- The agent sends `DataFrame` exec and file requests to the guest helper and forwards its responses to the data path. The guest helper serves the `GuestService` of the [protos contract](protos-ownsand-api-contract.md#guest-channel).
- The guest helper is part of every base image. For locally built macOS and Windows images, the image build installs it.

### Network policy

- The default mode `OUTBOUND` gives the guest outbound internet access through NAT on the host.
- In `OUTBOUND` mode, the agent drops every guest packet to these destinations: the host's own addresses on all interfaces; `0.0.0.0/8`, `10.0.0.0/8`, `100.64.0.0/10`, `127.0.0.0/8`, `169.254.0.0/16`, `172.16.0.0/12`, `192.168.0.0/16`, `224.0.0.0/4` and `240.0.0.0/4`; `::1/128`, `fc00::/7`, `fe80::/10` and `ff00::/8`; and the IPv4-mapped IPv6 forms of the IPv4 ranges. Inbound connections to the guest are not allowed.
- DNS: the agent serves DNS to the guest at the guest gateway address and resolves the queries through the host resolver. The gateway accepts only DNS from the guest. A name that resolves to a blocked address stays unreachable, because the packet filter applies after resolution.
- `NONE` mode attaches no network device to the guest. The guest helper still works through the hypervisor socket.
- Enforcement per host:
  - Linux: a tap device per sandbox with nftables rules in a table that the agent owns. The agent needs `CAP_NET_ADMIN`.
  - macOS: the built-in Virtualization.framework NAT attachment cannot filter LAN destinations. The agent attaches the guest network through a file-handle attachment to a userspace network stack in the agent that applies the policy. The network stack library is an open decision and a new dependency.
  - Windows: open decision between Host Networking Service ACL policies on the sandbox endpoint and a Windows Filtering Platform filter. Record the mechanism before the Windows runtime implementation.
- Network policy tests must show, on each host OS, that the guest reaches the internet in `OUTBOUND` mode and cannot reach the host, the LAN or the link-local range, and that a `NONE` guest has no network.

### TTL

The agent stores the idle and maximum deadlines of each sandbox in its state directory and enforces them as the [control plane contract](servers-ownsand-api-foundation.md#ttl) defines, also while it is disconnected. After a restart, it enforces deadlines that passed while it was stopped at once.

### Images

- `LINUX` base image: built and signed by the OwnSand release process and verified with the same built-in keys as agent releases. Record the distribution, the kernel source and the image format with the Linux runtime implementation.
- `MACOS` base image: built on the device from an Apple IPSW. The agent uses the latest supported restore image or an IPSW file that the admin names. The installation and the first-boot setup that installs the guest helper are open technical work.
- `WINDOWS` base image: built on the device from an ISO file that an admin supplies with `ownsand-agent image build --iso <path>`. The agent installs Windows unattended and installs the guest helper. Delino does not distribute the ISO or a product key.
- The organization is responsible for the license terms of its guest operating systems.
- Templates are snapshots with a name. A sandbox created from a template uses the snapshot's disk.

### Pause, resume and snapshots

- Pause stops the VM's vCPUs and keeps its memory. Resume continues it. On macOS, paused VMs end when the agent process ends.
- A snapshot captures the guest disk. Where the hypervisor supports it, the snapshot also captures the memory state; record per hypervisor which content a snapshot holds before its implementation. A sandbox created from a snapshot is a new sandbox with a new ID.
- Snapshots stay in the agent state directory by default.
- Export: the agent uploads the snapshot to R2 with the signed URLs that the control plane issues, in parts, and reports the SHA-256 of each part. Import: the agent downloads with signed URLs, checks the hashes and only then marks the snapshot ready. An exported snapshot can run only on a device with the same host OS and architecture.

### Data path

- The agent accepts a WebRTC data channel only for an `OpenSession` offer that it received from the control plane on the agent channel, and only from the DTLS fingerprint in that offer.
- The agent answers with its ICE candidates, including STUN-derived server-reflexive candidates. When ICE fails, the caller uses the relay, and the agent serves the same `DataFrame` requests from the agent channel.
- The agent never sends sandbox content anywhere except to the data channel of the session or to the relay.

### Device removal

When the agent receives a clean-up command or a removal notice from `Authenticate`, it stops and deletes every local sandbox, snapshot and locally built image, deletes the device key, reports the result when it is still connected, and exits enrolled state. `ownsand-agent reset` does the same without the control plane.

### Agent updates

- The control plane sends the target version on the agent channel according to the organization's [agent version policy](servers-ownsand-api-foundation.md#agent-version-policy).
- The agent downloads a signed release manifest and the release artifact for its platform. The manifest names the version, the platform, the SHA-256 of the artifact and the versions that are revoked. The agent verifies the manifest signature with a public key that is built into the agent, and verifies the artifact hash, before it installs anything. It rejects a release with a bad signature, a hash mismatch or a revoked version, and reports `FAILED`.
- The agent keeps a list of built-in public keys so that a release signed with the current key can introduce the next key.
- The agent installs an update only when no sandbox is `creating`, `running` or `paused` and no snapshot export or import runs. Until then, it reports `PENDING_IDLE` and keeps serving its sandboxes. It never stops a sandbox to install an update.
- A version lower than the running version is installed only when the organization policy pins it.
- The release signing tool and the key custody process are open decisions. Record them here before the first release.

## Storage

- State directory, readable only by the agent's service account: `/var/lib/ownsand-agent` on Linux, `/Library/Application Support/ownsand-agent` on macOS and `%ProgramData%\ownsand-agent` on Windows.
- Contents: the device ID, the Ed25519 private key, the control plane origin, the sandbox and snapshot records with their TTL deadlines, VM disks, memory state, snapshots, base images and the pending update.
- The agent never writes enrollment tokens, session credentials, signed URLs, exec content or file content to its logs.

## Security

- The device private key never leaves the device. Removal and `reset` delete it.
- Sandbox content stays in the VM, the state directory, the data channel, the relay stream or an explicit R2 export.
- The agent validates every guest path request inside the guest helper, so a guest path never touches the host file system.
- The agent connects only outbound, to the control plane origin, the STUN service, R2 signed URLs and release downloads. It opens no inbound listening port on the host network.

## Logging

- Structured JSON logs with `log/slog` to the platform service log (journald, the unified log or the Windows event log, or a log file in the state directory; record the choice with the implementation).
- Events: enrollment result, authentication result, channel connect and disconnect, capability report, command ID and result, sandbox state change, TTL expiry, network setup result, snapshot export and import progress, data path transport selection, and update verification and installation.
- Never log enrollment tokens, session credentials, private keys, signed URLs, SDP bodies, exec commands, environment values, exec output or file contents.

## Build and Test

- From the repository root: `gofmt` check, `go vet ./...` and `go test ./...`, according to `cmds/AGENTS.md`.
- CI runs on Linux only and excludes macOS runners from pull requests and main pushes, according to the [workflow contract](repository-workflow-contract.md). Cross-compile the macOS and Windows code paths with `GOOS` on Linux so that CI checks that they build; behavior on macOS and Windows needs device acceptance.
- Unit tests: TTL clocks, update verification (bad signature, hash mismatch, revoked version, update while sandboxes run, pinned version), state report reconciliation, network policy rule generation and token handling.
- Device acceptance on real Linux, macOS and Windows hosts: the full lifecycle, the network policy tests, the relay fallback, disconnect and reconnect, snapshot export and restore on a second device, removal clean-up and signed updates. Record the results in pull requests and Linear issues, separate from CI.

## Dependencies and Integrations

- Upstream: the [control plane](servers-ownsand-api-foundation.md), the STUN service, Cloudflare R2 signed URLs, the release manifest host.
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
