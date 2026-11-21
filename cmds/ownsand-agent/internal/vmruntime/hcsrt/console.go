// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"io"
	"net"
	"time"
)

// consoleRedialDelay is the wait before the console copy dials the COM1 pipe
// again after a dial failure or after a copy that ended while the VM runs.
const consoleRedialDelay = 100 * time.Millisecond

// consoleDialer connects to the COM1 pipe of a VM. Production code uses
// winio.DialPipe; tests substitute it so they need no Hyper-V VM.
type consoleDialer func() (net.Conn, error)

// copyConsoleLoop copies the COM1 output to out and dials again whenever a
// connection ends or a dial fails, until stopping or exited closes. Closing
// either channel closes the live connection, so the blocked copy returns.
func copyConsoleLoop(dial consoleDialer, out io.Writer, stopping, exited <-chan struct{}) {
	for {
		if conn, err := dial(); err == nil {
			copyConsoleConn(out, conn, stopping, exited)
		}
		select {
		case <-stopping:
			return
		case <-exited:
			return
		case <-time.After(consoleRedialDelay):
		}
	}
}

// copyConsoleConn copies one pipe connection to out and closes it. A closer
// goroutine closes the connection when the VM stops, which ends the copy.
// The closer ends with the copy as well, so a pipe that ends while the VM
// still runs leaves no goroutine behind: copyConsoleConn returns only after
// the closer has returned.
func copyConsoleConn(out io.Writer, conn net.Conn, stopping, exited <-chan struct{}) {
	copied := make(chan struct{})
	closerDone := make(chan struct{})
	go closeConsoleOnStop(conn, stopping, exited, copied, closerDone)
	_, _ = io.Copy(out, conn)
	close(copied)
	conn.Close()
	<-closerDone
}

// closeConsoleOnStop closes conn when the VM stops, and returns without
// closing when the copy ends first, because the copy closes the connection
// itself.
func closeConsoleOnStop(conn net.Conn, stopping, exited, copied <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	select {
	case <-stopping:
		conn.Close()
	case <-exited:
		conn.Close()
	case <-copied:
	}
}
