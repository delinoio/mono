// Package dnsfwd serves DNS to a guest at its gateway address and forwards
// the queries to the host resolver. It relays DNS messages without parsing
// them; the packet filter applies after resolution, so a name that resolves
// to a blocked address stays unreachable.
package dnsfwd

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	queryTimeout   = 5 * time.Second
	tcpIdleTimeout = 30 * time.Second
	// maxInflight bounds the concurrent queries of one guest.
	maxInflight = 128
)

// Upstreams reads the nameservers of a resolv.conf file. With
// systemd-resolved, this is the local stub resolver.
func Upstreams(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) >= 2 && fields[0] == "nameserver" {
			if a, err := netip.ParseAddr(fields[1]); err == nil {
				out = append(out, netip.AddrPortFrom(a, 53).String())
			}
		}
	}
	return out
}

// Forwarder serves one guest gateway address.
type Forwarder struct {
	udp       net.PacketConn
	tcp       net.Listener
	upstreams []string
	log       *slog.Logger
	sem       chan struct{}
	wg        sync.WaitGroup
	cancel    context.CancelFunc
}

// Start listens on addr (UDP and TCP) and forwards to upstreams.
func Start(addr netip.AddrPort, upstreams []string, log *slog.Logger) (*Forwarder, error) {
	if len(upstreams) == 0 {
		return nil, errors.New("no upstream DNS resolver is configured on the host")
	}
	pc, err := net.ListenPacket("udp", addr.String())
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr.String())
	if err != nil {
		_ = pc.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &Forwarder{udp: pc, tcp: ln, upstreams: upstreams, log: log, sem: make(chan struct{}, maxInflight), cancel: cancel}
	f.wg.Add(2)
	go f.serveUDP(ctx)
	go f.serveTCP(ctx)
	return f, nil
}

// UDPAddr returns the bound UDP address (tests).
func (f *Forwarder) UDPAddr() net.Addr { return f.udp.LocalAddr() }

// TCPAddr returns the bound TCP address (tests).
func (f *Forwarder) TCPAddr() net.Addr { return f.tcp.Addr() }

// Close stops the forwarder.
func (f *Forwarder) Close() error {
	f.cancel()
	err := errors.Join(f.udp.Close(), f.tcp.Close())
	f.wg.Wait()
	return err
}

func (f *Forwarder) acquire() bool {
	select {
	case f.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (f *Forwarder) release() { <-f.sem }

func (f *Forwarder) serveUDP(ctx context.Context) {
	defer f.wg.Done()
	buf := make([]byte, 65535)
	for {
		n, from, err := f.udp.ReadFrom(buf)
		if err != nil {
			return
		}
		if !f.acquire() {
			continue // over the in-flight limit: drop, the guest retries
		}
		q := append([]byte(nil), buf[:n]...)
		go func() {
			defer f.release()
			resp, err := f.exchangeUDP(ctx, q)
			if err != nil {
				f.log.Debug("dns forward failed", "error", err)
				return
			}
			_, _ = f.udp.WriteTo(resp, from)
		}()
	}
}

func (f *Forwarder) exchangeUDP(ctx context.Context, q []byte) ([]byte, error) {
	var lastErr error
	for _, up := range f.upstreams {
		d := net.Dialer{Timeout: queryTimeout}
		c, err := d.DialContext(ctx, "udp", up)
		if err != nil {
			lastErr = err
			continue
		}
		_ = c.SetDeadline(time.Now().Add(queryTimeout))
		if _, err := c.Write(q); err != nil {
			_ = c.Close()
			lastErr = err
			continue
		}
		buf := make([]byte, 65535)
		n, err := c.Read(buf)
		_ = c.Close()
		if err != nil {
			lastErr = err
			continue
		}
		return buf[:n], nil
	}
	return nil, lastErr
}

func (f *Forwarder) serveTCP(ctx context.Context) {
	defer f.wg.Done()
	for {
		c, err := f.tcp.Accept()
		if err != nil {
			return
		}
		if !f.acquire() {
			_ = c.Close()
			continue
		}
		go func() {
			defer f.release()
			defer c.Close()
			f.proxyTCP(ctx, c)
		}()
	}
}

func (f *Forwarder) proxyTCP(ctx context.Context, c net.Conn) {
	var up net.Conn
	var err error
	for _, u := range f.upstreams {
		d := net.Dialer{Timeout: queryTimeout}
		if up, err = d.DialContext(ctx, "tcp", u); err == nil {
			break
		}
	}
	if up == nil {
		return
	}
	defer up.Close()
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		buf := make([]byte, 16<<10)
		for {
			_ = src.SetReadDeadline(time.Now().Add(tcpIdleTimeout))
			n, err := src.Read(buf)
			if n > 0 {
				if _, werr := dst.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		done <- struct{}{}
	}
	go pipe(up, c)
	go pipe(c, up)
	select {
	case <-done:
	case <-ctx.Done():
	}
}
