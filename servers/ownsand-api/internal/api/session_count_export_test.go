// SPDX-License-Identifier: FSL-1.1-ALv2

package api

// OpenSessionCount reports the number of sessions that the control plane
// tracks as pending or open. It is exported for the external api_test
// package, which cannot reach the unexported session map.
func (p *DataPlane) OpenSessionCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sessions)
}
