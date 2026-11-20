// SPDX-License-Identifier: FSL-1.1-ALv2

// Package stunserver answers STUN binding requests (RFC 8489) on UDP without
// authentication, with github.com/pion/stun/v3. It never relays media or
// data. Responses are rate-limited per source address, because a response is
// larger than the smallest request and could be reflected toward a spoofed
// address.
package stunserver

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"github.com/pion/stun/v3"

	"github.com/delinoio/mono/servers/ownsand-api/internal/ratelimit"
)

// Limits per source IP address.
const (
	// MaxRequestBytes drops larger datagrams; a binding request needs far less.
	MaxRequestBytes = 548
	// RequestsPerSecond and Burst bound the responses to one source IP. A
	// response is at most ResponseBytesMax bytes, so one source gets at most
	// RequestsPerSecond*ResponseBytesMax bytes per second after the burst.
	RequestsPerSecond = 5
	Burst             = 20
	ResponseBytesMax  = 64
	statsInterval     = time.Minute
)

// Server answers STUN binding requests.
type Server struct {
	Conn    net.PacketConn
	Logger  *slog.Logger
	Limiter *ratelimit.Limiter

	requests, responses, limited, dropped atomic.Int64
}

// New returns a server for conn with the default per-source limits.
func New(conn net.PacketConn, logger *slog.Logger) *Server {
	return &Server{Conn: conn, Logger: logger, Limiter: ratelimit.New(RequestsPerSecond, Burst, nil)}
}

// Serve answers requests until ctx ends or the connection closes.
func (s *Server) Serve(ctx context.Context) error {
	go s.stats(ctx)
	go func() {
		<-ctx.Done()
		_ = s.Conn.Close()
	}()
	buf := make([]byte, 1500)
	for {
		n, addr, err := s.Conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		s.handle(buf[:n], addr)
	}
}

func (s *Server) handle(b []byte, addr net.Addr) {
	udp, ok := addr.(*net.UDPAddr)
	if !ok || len(b) > MaxRequestBytes || !stun.IsMessage(b) {
		s.dropped.Add(1)
		return
	}
	req := &stun.Message{Raw: append([]byte(nil), b...)}
	if err := req.Decode(); err != nil || req.Type != stun.BindingRequest {
		s.dropped.Add(1)
		return
	}
	s.requests.Add(1)
	if !s.Limiter.Allow(udp.IP.String()) {
		s.limited.Add(1)
		return
	}
	resp, err := stun.Build(stun.NewTransactionIDSetter(req.TransactionID), stun.BindingSuccess,
		&stun.XORMappedAddress{IP: udp.IP, Port: udp.Port}, stun.Fingerprint)
	if err != nil || len(resp.Raw) > ResponseBytesMax {
		s.dropped.Add(1)
		return
	}
	if _, err := s.Conn.WriteTo(resp.Raw, addr); err == nil {
		s.responses.Add(1)
	}
}

// stats logs aggregate counts, never one record per request.
func (s *Server) stats(ctx context.Context) {
	t := time.NewTicker(statsInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		req, resp, lim, drop := s.requests.Swap(0), s.responses.Swap(0), s.limited.Swap(0), s.dropped.Swap(0)
		if req+drop > 0 {
			s.Logger.Info("stun_stats", slog.Int64("requests", req), slog.Int64("responses", resp), slog.Int64("rate_limited", lim), slog.Int64("dropped", drop))
		}
	}
}
