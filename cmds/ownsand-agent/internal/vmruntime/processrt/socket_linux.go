// SPDX-License-Identifier: FSL-1.1-ALv2

package processrt

// socketPath returns an abstract unix socket name (a leading @), because a
// path under a deep state directory can exceed the 108-byte limit of unix
// socket paths. Abstract sockets are visible to every process in the
// network namespace; this is acceptable only because the process runtime is
// a test runtime.
func socketPath(_ string, sandboxID string) string {
	return "@ownsand-guest-" + sandboxID
}
