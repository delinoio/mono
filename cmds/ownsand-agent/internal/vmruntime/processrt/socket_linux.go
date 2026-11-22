// SPDX-License-Identifier: FSL-1.1-ALv2

package processrt

// newSocket returns an abstract unix socket name (a leading @), because a
// path under a deep state directory can exceed the 108-byte limit of unix
// socket paths. Abstract sockets are visible to every process in the
// network namespace; this is acceptable only because the process runtime is
// a test runtime. An abstract socket has no filesystem entry, so newSocket
// returns no socket directory.
func newSocket(sandboxID string) (address, dir string, err error) {
	return "@ownsand-guest-" + sandboxID, "", nil
}
