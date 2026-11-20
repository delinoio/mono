// SPDX-License-Identifier: FSL-1.1-ALv2

// Package stun answers STUN binding requests (RFC 8489) on UDP with the
// pion STUN library: the response carries the datagram's source address in
// XOR-MAPPED-ADDRESS. There is no TURN and no authentication. Responses are
// rate-limited per source address with a byte budget, because a response
// can be larger than its request and the source address can be spoofed.
// The owning contract is the STUN section of
// docs/servers-ownsand-api-foundation.md.
package stun

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	pionstun "github.com/pion/stun/v3"

	"github.com/delinoio/mono/servers/ownsand-api/internal/ratelimit"
)

// maxDatagram bounds a received datagram.
const maxDatagram = 1500

// statsInterval is how often the server logs aggregate counts.
const statsInterval = time.Minute

// Server is a STUN binding server.
type Server struct {
	Logger *slog.Logger
	// Limiter budgets response bytes per source IP.
	Limiter *ratelimit.Limiter

	requests, responses, limited, invalid atomic.Int64
}

// New returns a server whose responses to one source address stay within
// bytesPerSecond with a burst of burst bytes.
func New(logger *slog.Logger, bytesPerSecond, burst int) *Server {
	return &Server{Logger: logger, Limiter: ratelimit.New(float64(bytesPerSecond), burst, nil)}
}

// Respond returns the binding response for a datagram from src, or nil when
// the datagram is not a binding request.
func Respond(datagram []byte, src *net.UDPAddr) ([]byte, error) {
	if !pionstun.IsMessage(datagram) {
		return nil, errors.New("not a STUN message")
	}
	req := &pionstun.Message{Raw: append([]byte(nil), datagram...)}
	if err := req.Decode(); err != nil {
		return nil, err
	}
	if req.Type != pionstun.BindingRequest {
		return nil, errors.New("not a binding request")
	}
	resp, err := pionstun.Build(
		pionstun.NewTransactionIDSetter(req.TransactionID),
		pionstun.BindingSuccess,
		&pionstun.XORMappedAddress{IP: src.IP, Port: src.Port},
		pionstun.Fingerprint,
	)
	if err != nil {
		return nil, err
	}
	return resp.Raw, nil
}

// Serve answers binding requests on pc until ctx ends.
func (s *Server) Serve(ctx context.Context, pc net.PacketConn) error {
	go func() {
		<-ctx.Done()
		_ = pc.Close()
	}()
	go s.logStats(ctx)
	buf := make([]byte, maxDatagram)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		src, ok := addr.(*net.UDPAddr)
		if !ok {
			continue
		}
		s.requests.Add(1)
		resp, err := Respond(buf[:n], src)
		if err != nil {
			s.invalid.Add(1)
			continue
		}
		if ok, _ := s.Limiter.Allow(src.IP.String(), float64(len(resp))); !ok {
			s.limited.Add(1)
			continue
		}
		if _, err := pc.WriteTo(resp, src); err == nil {
			s.responses.Add(1)
		}
	}
}

func (s *Server) logStats(ctx context.Context) {
	t := time.NewTicker(statsInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		req, resp, lim, inv := s.requests.Swap(0), s.responses.Swap(0), s.limited.Swap(0), s.invalid.Swap(0)
		if req > 0 {
			s.Logger.Info("stun_stats", slog.Int64("requests", req), slog.Int64("responses", resp), slog.Int64("rate_limited", lim), slog.Int64("invalid", inv))
		}
	}
}

// Stats returns the counts since the last stats record (tests).
func (s *Server) Stats() (requests, responses, limited int64) {
	return s.requests.Load(), s.responses.Load(), s.limited.Load()
}
