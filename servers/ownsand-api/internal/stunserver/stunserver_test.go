// SPDX-License-Identifier: FSL-1.1-ALv2

package stunserver

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/pion/stun/v3"

	"github.com/delinoio/mono/servers/ownsand-api/internal/ratelimit"
)

func TestBindingAndRateLimit(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := New(pc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// A frozen clock: unanswered requests wait for a read timeout, which
	// would otherwise refill the bucket.
	frozen := time.Now()
	s.Limiter = ratelimit.New(RequestsPerSecond, Burst, func() time.Time { return frozen })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Serve(ctx) }()

	client, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ask := func() (*stun.Message, bool) {
		req := stun.MustBuild(stun.TransactionID, stun.BindingRequest, stun.Fingerprint)
		if _, err := client.WriteTo(req.Raw, pc.LocalAddr()); err != nil {
			t.Fatal(err)
		}
		_ = client.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		buf := make([]byte, 1500)
		n, err := client.Read(buf)
		if err != nil {
			return nil, false
		}
		resp := &stun.Message{Raw: buf[:n]}
		if err := resp.Decode(); err != nil {
			t.Fatal(err)
		}
		if resp.TransactionID != req.TransactionID {
			t.Fatal("transaction ID")
		}
		return resp, true
	}
	resp, ok := ask()
	if !ok || resp.Type != stun.BindingSuccess {
		t.Fatal("no binding response")
	}
	var mapped stun.XORMappedAddress
	if err := mapped.GetFrom(resp); err != nil {
		t.Fatal(err)
	}
	local := client.LocalAddr().(*net.UDPAddr)
	if !mapped.IP.Equal(local.IP) || mapped.Port != local.Port {
		t.Fatalf("mapped %v, client %v", mapped, local)
	}
	if err := stun.Fingerprint.Check(resp); err != nil {
		t.Fatal(err)
	}
	answered := 1
	for i := 0; i < Burst+5; i++ {
		if _, ok := ask(); ok {
			answered++
		}
	}
	if answered != Burst {
		t.Fatalf("rate limit: %d responses", answered)
	}
	if s.limited.Load() == 0 {
		t.Fatal("no rate-limited requests counted")
	}
	// Non-STUN datagrams are dropped silently.
	_, _ = client.WriteTo([]byte("hello"), pc.LocalAddr())
	time.Sleep(50 * time.Millisecond)
	if s.dropped.Load() == 0 {
		t.Fatal("garbage not dropped")
	}
}
