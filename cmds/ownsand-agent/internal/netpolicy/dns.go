// SPDX-License-Identifier: Apache-2.0

package netpolicy

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"
)

// DNSForwarder serves DNS to one guest at its gateway address and resolves
// through the host resolver. It forwards whole messages without parsing
// them, over UDP and TCP.
type DNSForwarder struct {
	udp      *net.UDPConn
	tcp      *net.TCPListener
	upstream string
	logger   *slog.Logger
	wg       sync.WaitGroup
	closed   chan struct{}
	mu       sync.Mutex
	conns    map[net.Conn]bool
}

// HostResolver returns the first name server of /etc/resolv.conf as
// host:port, or 127.0.0.53:53 (systemd-resolved) when none is listed.
func HostResolver(path string) string {
	f, err := os.Open(path)
	if err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) >= 2 && fields[0] == "nameserver" {
				if a, err := netip.ParseAddr(fields[1]); err == nil {
					return netip.AddrPortFrom(a.WithZone(""), 53).String()
				}
			}
		}
	}
	return "127.0.0.53:53"
}

// StartDNS listens on listen (UDP and TCP) and forwards to upstream.
func StartDNS(listen netip.AddrPort, upstream string, logger *slog.Logger) (*DNSForwarder, error) {
	udp, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(listen))
	if err != nil {
		return nil, err
	}
	tcp, err := net.ListenTCP("tcp", net.TCPAddrFromAddrPort(netip.AddrPortFrom(listen.Addr(), udp.LocalAddr().(*net.UDPAddr).AddrPort().Port())))
	if err != nil {
		udp.Close()
		return nil, err
	}
	f := &DNSForwarder{udp: udp, tcp: tcp, upstream: upstream, logger: logger, closed: make(chan struct{}), conns: map[net.Conn]bool{}}
	f.wg.Add(2)
	go f.serveUDP()
	go f.serveTCP()
	return f, nil
}

// Addr returns the UDP listen address.
func (f *DNSForwarder) Addr() netip.AddrPort {
	return f.udp.LocalAddr().(*net.UDPAddr).AddrPort()
}

// Close stops the forwarder.
func (f *DNSForwarder) Close() {
	select {
	case <-f.closed:
		return
	default:
	}
	close(f.closed)
	f.udp.Close()
	f.tcp.Close()
	f.mu.Lock()
	for c := range f.conns {
		c.Close()
	}
	f.mu.Unlock()
	f.wg.Wait()
}

func (f *DNSForwarder) serveUDP() {
	defer f.wg.Done()
	buf := make([]byte, 65535)
	for {
		n, from, err := f.udp.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		query := append([]byte(nil), buf[:n]...)
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			c, err := net.DialTimeout("udp", f.upstream, 5*time.Second)
			if err != nil {
				return
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := c.Write(query); err != nil {
				return
			}
			resp := make([]byte, 65535)
			m, err := c.Read(resp)
			if err != nil {
				return
			}
			f.udp.WriteToUDPAddrPort(resp[:m], from)
		}()
	}
}

func (f *DNSForwarder) serveTCP() {
	defer f.wg.Done()
	for {
		c, err := f.tcp.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns[c] = true
		f.mu.Unlock()
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			defer func() {
				f.mu.Lock()
				delete(f.conns, c)
				f.mu.Unlock()
				c.Close()
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var d net.Dialer
			up, err := d.DialContext(ctx, "tcp", f.upstream)
			if err != nil {
				return
			}
			defer up.Close()
			deadline := time.Now().Add(30 * time.Second)
			c.SetDeadline(deadline)
			up.SetDeadline(deadline)
			done := make(chan struct{})
			go func() {
				io.Copy(up, c)
				if tc, ok := up.(*net.TCPConn); ok {
					tc.CloseWrite()
				}
				close(done)
			}()
			io.Copy(c, up)
			<-done
		}()
	}
}
