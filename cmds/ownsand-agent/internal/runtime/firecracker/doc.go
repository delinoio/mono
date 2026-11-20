// Package firecracker is the Linux runtime of the agent: one Firecracker
// microVM process per sandbox on KVM, with a tap device and the nftables
// network policy, vsock to the guest helper, pause and resume, and full
// snapshots (disk and memory state).
//
// The runtime itself builds only on Linux. The protocol helpers (the
// Firecracker API client, the vsock handshake, subnet allocation and base
// image verification) build on every OS so their tests run everywhere.
package firecracker

// Version is the pinned Firecracker release. The agent refuses another
// version, because snapshots and the API depend on it.
const Version = "v1.17.0"

// GuestVsockCID is the guest context ID of every sandbox.
const GuestVsockCID = 3
