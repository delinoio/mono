// SPDX-License-Identifier: FSL-1.1-ALv2

package directory

// RefreshWaiters returns how many callers started or joined the token refresh
// that is running, and 0 when none runs. Tests use it to wait until callers
// have joined a refresh without sleeping.
func (l *Logto) RefreshWaiters() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inflight == nil {
		return 0
	}
	return l.inflight.waiters
}
