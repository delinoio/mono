// SPDX-License-Identifier: FSL-1.1-ALv2

package ops

// ExecContextErr returns the error of the forwarding context of the exec and
// whether the exec is kept. It is nil while forwarding is still running.
func (d *Dispatcher) ExecContextErr(execID string) (err error, kept bool) {
	d.mu.Lock()
	op, ok := d.execs[execID]
	d.mu.Unlock()
	if !ok {
		return nil, false
	}
	return op.ctx.Err(), true
}
