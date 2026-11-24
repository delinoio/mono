// SPDX-License-Identifier: FSL-1.1-ALv2

package stun

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	pionstun "github.com/pion/stun/v3"

	"github.com/delinoio/mono/servers/ownsand-api/internal/config"
)

// A 20-byte STUN header and four bytes per empty attribute fill maxDatagram.
const maxUnknownRequiredAttributes = (maxDatagram - 20) / 4

func bindingRequestWithUnknownRequired(count int) *pionstun.Message {
	setters := []pionstun.Setter{pionstun.TransactionID, pionstun.BindingRequest}
	for i := range count {
		setters = append(setters, pionstun.RawAttribute{Type: 0x0100 + pionstun.AttrType(i)})
	}
	return pionstun.MustBuild(setters...)
}

// The smallest accepted burst must admit every response the server builds:
// the limiter drops a response larger than the whole burst.
func TestResponsesFitMinimumBurst(t *testing.T) {
	plain := pionstun.MustBuild(pionstun.TransactionID, pionstun.BindingRequest, pionstun.Fingerprint)
	unknown := pionstun.MustBuild(pionstun.TransactionID, pionstun.BindingRequest,
		pionstun.RawAttribute{Type: 0x0030, Value: []byte{1, 2, 3, 4}}, pionstun.Fingerprint)
	ipv4 := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 4000}
	ipv6 := &net.UDPAddr{IP: net.ParseIP("2001:db8::1"), Port: 4000}
	largest := bindingRequestWithUnknownRequired(maxUnknownRequiredAttributes)
	if len(largest.Raw) != maxDatagram {
		t.Fatalf("largest request is %d bytes, want %d", len(largest.Raw), maxDatagram)
	}
	for name, tc := range map[string]struct {
		req      *pionstun.Message
		src      *net.UDPAddr
		wantSize int
	}{
		"ipv4 success":     {plain, ipv4, 40},
		"ipv6 success":     {plain, ipv6, 52},
		"ipv4 420":         {unknown, ipv4, 64},
		"ipv6 420":         {unknown, ipv6, 64},
		"420 with 2 types": {bindingRequestWithUnknownRequired(2), ipv4, 64},
		"420 with 3 types": {bindingRequestWithUnknownRequired(3), ipv4, 68},
		"largest 420":      {largest, ipv4, 800},
	} {
		resp, err := Respond(tc.req.Raw, tc.src)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(resp) != tc.wantSize {
			t.Errorf("%s: response is %d bytes, want %d", name, len(resp), tc.wantSize)
		}
		if len(resp) > config.MinSTUNRateBurstBytes {
			t.Errorf("%s: response of %d bytes exceeds the %d-byte minimum burst", name, len(resp), config.MinSTUNRateBurstBytes)
		}
	}
}

func TestResponsesFitMinimumBurstOverUDP(t *testing.T) {
	for _, tc := range []struct {
		name     string
		count    int
		wantSize int
	}{
		{"3 unknown types", 3, 68},
		{"maximum unknown types", maxUnknownRequiredAttributes, 800},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			s := New(slog.New(slog.NewJSONHandler(io.Discard, nil)), config.DefaultSTUNRateBytesPerSecond, config.MinSTUNRateBurstBytes)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- s.Serve(ctx, pc) }()
			defer func() {
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()

			client, err := net.DialUDP("udp4", nil, pc.LocalAddr().(*net.UDPAddr))
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			req := bindingRequestWithUnknownRequired(tc.count)
			if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := client.Write(req.Raw); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, maxDatagram)
			n, err := client.Read(buf)
			if err != nil {
				t.Fatal(err)
			}
			if n != tc.wantSize {
				t.Fatalf("response is %d bytes, want %d", n, tc.wantSize)
			}
			resp := &pionstun.Message{Raw: buf[:n]}
			if err := resp.Decode(); err != nil {
				t.Fatal(err)
			}
			var code pionstun.ErrorCodeAttribute
			if resp.Type != pionstun.BindingError || resp.TransactionID != req.TransactionID ||
				code.GetFrom(resp) != nil || code.Code != pionstun.CodeUnknownAttribute {
				t.Fatalf("response type %v, transaction %x, code %v", resp.Type, resp.TransactionID, code)
			}
			if reqs, resps, limited := waitForOutcomes(s.Stats, 1, 5*time.Second); reqs != 1 || resps != 1 || limited != 0 {
				t.Fatalf("requests %d, responses %d, limited %d", reqs, resps, limited)
			}
		})
	}
}

func TestBindingResponseCarriesSourceAddress(t *testing.T) {
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := New(slog.New(slog.NewJSONHandler(io.Discard, nil)), 2048, 8192)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, pc) }()
	defer func() { cancel(); <-done }()

	client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	req := pionstun.MustBuild(pionstun.TransactionID, pionstun.BindingRequest, pionstun.Fingerprint)
	if _, err := client.WriteTo(req.Raw, pc.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1500)
	n, _, err := client.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	resp := &pionstun.Message{Raw: buf[:n]}
	if err := resp.Decode(); err != nil {
		t.Fatal(err)
	}
	var xor pionstun.XORMappedAddress
	if err := xor.GetFrom(resp); err != nil {
		t.Fatal(err)
	}
	local := client.LocalAddr().(*net.UDPAddr)
	if resp.Type != pionstun.BindingSuccess || resp.TransactionID != req.TransactionID || !xor.IP.Equal(local.IP) || xor.Port != local.Port {
		t.Fatalf("response %v maps %v, want %v", resp.Type, xor, local)
	}
}

// fakePacketConn is an in-memory net.PacketConn. ReadFrom returns the
// datagrams queued with enqueue, each with the source address the test
// chose, and WriteTo records the destination of every response. It lets a
// test use several source IPs without binding loopback aliases, which are
// not available on every CI platform (macOS binds only 127.0.0.1).
type fakePacketConn struct {
	in     chan fakeDatagram
	closed chan struct{}
	once   sync.Once

	mu     sync.Mutex
	writes []net.Addr
}

type fakeDatagram struct {
	payload []byte
	src     net.Addr
}

func newFakePacketConn() *fakePacketConn {
	return &fakePacketConn{in: make(chan fakeDatagram, 64), closed: make(chan struct{})}
}

func (c *fakePacketConn) enqueue(payload []byte, src *net.UDPAddr) {
	c.in <- fakeDatagram{payload: payload, src: src}
}

func (c *fakePacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	select {
	case d := <-c.in:
		return copy(p, d.payload), d.src, nil
	case <-c.closed:
		return 0, nil, net.ErrClosed
	}
}

func (c *fakePacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes = append(c.writes, addr)
	return len(p), nil
}

func (c *fakePacketConn) writtenTo() []net.Addr {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]net.Addr(nil), c.writes...)
}

func (c *fakePacketConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *fakePacketConn) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(192, 0, 2, 100), Port: 3478}
}
func (c *fakePacketConn) SetDeadline(time.Time) error      { return nil }
func (c *fakePacketConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakePacketConn) SetWriteDeadline(time.Time) error { return nil }

// The byte budget is per source IP: one source that exhausts its budget is
// dropped, and a different source still gets a response. A single global
// bucket would drop the second source and fail this test.
func TestRateLimitPerSource(t *testing.T) {
	pc := newFakePacketConn()
	// A budget of about two responses and almost no refill.
	s := New(slog.New(slog.NewJSONHandler(io.Discard, nil)), 1, 100)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, pc) }()
	defer func() { cancel(); <-done }()

	first := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 4000}
	second := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 2), Port: 4000}
	for i := 0; i < 10; i++ {
		req := pionstun.MustBuild(pionstun.TransactionID, pionstun.BindingRequest)
		pc.enqueue(req.Raw, first)
	}
	req, resp, limited := waitForOutcomes(s.Stats, 10, 5*time.Second)
	if req != 10 || resp == 0 || resp > 3 || limited != req-resp {
		t.Fatalf("first source: requests %d, responses %d, limited %d", req, resp, limited)
	}

	// The first source has no budget left; a different source is unaffected.
	other := pionstun.MustBuild(pionstun.TransactionID, pionstun.BindingRequest)
	pc.enqueue(other.Raw, second)
	req, resp2, limited2 := waitForOutcomes(s.Stats, 11, 5*time.Second)
	if req != 11 || resp2 != resp+1 || limited2 != limited {
		t.Fatalf("second source: requests %d, responses %d (was %d), limited %d (was %d); want one more response and no more drops",
			req, resp2, resp, limited2, limited)
	}
	var toFirst, toSecond int
	for _, addr := range pc.writtenTo() {
		switch addr.String() {
		case first.String():
			toFirst++
		case second.String():
			toSecond++
		default:
			t.Fatalf("response sent to unexpected address %v", addr)
		}
	}
	if int64(toFirst) != resp || toSecond != 1 {
		t.Fatalf("responses addressed to %v: %d, to %v: %d; want %d and 1", first, toFirst, second, toSecond, resp)
	}
}

// waitForOutcomes polls stats until at least want outcomes (responses plus
// limited drops) are recorded, or the timeout passes. Request receipt is not a
// completion signal because the server records the outcome after it counts the
// request. It returns the last snapshot so callers assert on one consistent sample.
func waitForOutcomes(stats func() (int64, int64, int64), want int64, timeout time.Duration) (req, resp, limited int64) {
	deadline := time.Now().Add(timeout)
	for {
		req, resp, limited = stats()
		if resp+limited >= want || !time.Now().Before(deadline) {
			return req, resp, limited
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWaitForOutcomes(t *testing.T) {
	samples := [][3]int64{{10, 5, 4}, {10, 5, 4}, {10, 6, 4}}
	calls := 0
	stats := func() (int64, int64, int64) {
		i := calls
		if i >= len(samples) {
			i = len(samples) - 1
		}
		calls++
		return samples[i][0], samples[i][1], samples[i][2]
	}
	req, resp, limited := waitForOutcomes(stats, 10, 5*time.Second)
	if req != 10 || resp != 6 || limited != 4 || calls != 3 {
		t.Fatalf("got %d/%d/%d after %d calls", req, resp, limited, calls)
	}
	// Missing outcomes return the incomplete sample at the deadline.
	req, resp, limited = waitForOutcomes(func() (int64, int64, int64) { return 10, 5, 4 }, 10, 30*time.Millisecond)
	if resp+limited != 9 || req != 10 {
		t.Fatalf("timeout sample %d/%d/%d", req, resp, limited)
	}
}

func TestRejectsNonBinding(t *testing.T) {
	if _, err := Respond([]byte("hello"), &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 5}); err == nil {
		t.Fatal("garbage answered")
	}
	ind := pionstun.MustBuild(pionstun.TransactionID, pionstun.NewType(pionstun.MethodBinding, pionstun.ClassIndication))
	if _, err := Respond(ind.Raw, &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 5}); err == nil {
		t.Fatal("indication answered")
	}
}

func TestRejectsReservedHeaderBits(t *testing.T) {
	src := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 5}
	req := pionstun.MustBuild(pionstun.TransactionID, pionstun.BindingRequest, pionstun.Fingerprint)
	if resp, err := Respond(req.Raw, src); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	} else {
		m := &pionstun.Message{Raw: resp}
		var xor pionstun.XORMappedAddress
		if err := m.Decode(); err != nil || m.Type != pionstun.BindingSuccess || m.TransactionID != req.TransactionID ||
			xor.GetFrom(m) != nil || !xor.IP.Equal(src.IP) || xor.Port != src.Port || !m.Contains(pionstun.AttrFingerprint) {
			t.Fatalf("bad response %v %v", m, err)
		}
	}
	for _, bits := range []byte{0x40, 0x80, 0xC0} {
		raw := append([]byte(nil), req.Raw...)
		raw[0] |= bits
		if resp, err := Respond(raw, src); err == nil || resp != nil {
			t.Fatalf("bits %#x answered: %v %v", bits, resp, err)
		}
	}
}

func TestServeCountsReservedHeaderBitsInvalid(t *testing.T) {
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := New(slog.New(slog.NewJSONHandler(io.Discard, nil)), 2048, 8192)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, pc) }()
	defer func() { cancel(); <-done }()

	client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	req := pionstun.MustBuild(pionstun.TransactionID, pionstun.BindingRequest, pionstun.Fingerprint)
	req.Raw[0] |= 0x80
	if _, err := client.WriteTo(req.Raw, pc.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, _, err := client.ReadFrom(make([]byte, 1500)); err == nil {
		t.Fatalf("got %d response bytes", n)
	}
	deadline := time.Now().Add(2 * time.Second)
	for s.invalid.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("invalid = %d, want 1", s.invalid.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUnknownRequiredAttributesGet420(t *testing.T) {
	src := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 5}
	build := func(types ...pionstun.AttrType) *pionstun.Message {
		setters := []pionstun.Setter{pionstun.TransactionID, pionstun.BindingRequest}
		for _, at := range types {
			at := at
			setters = append(setters, pionstun.RawAttribute{Type: at, Value: []byte{1, 2, 3, 4}})
		}
		setters = append(setters, pionstun.Fingerprint)
		return pionstun.MustBuild(setters...)
	}

	req := build(0x0030, 0x8030, 0x0031)
	raw, err := Respond(req.Raw, src)
	if err != nil {
		t.Fatal(err)
	}
	m := &pionstun.Message{Raw: raw}
	if err := m.Decode(); err != nil {
		t.Fatal(err)
	}
	var code pionstun.ErrorCodeAttribute
	var unknown pionstun.UnknownAttributes
	if m.Type != pionstun.BindingError || m.TransactionID != req.TransactionID ||
		code.GetFrom(m) != nil || code.Code != pionstun.CodeUnknownAttribute ||
		unknown.GetFrom(m) != nil || len(unknown) != 2 || unknown[0] != 0x0030 || unknown[1] != 0x0031 ||
		!m.Contains(pionstun.AttrFingerprint) || pionstun.Fingerprint.Check(m) != nil {
		t.Fatalf("bad error response %v code %v unknown %v", m, code, unknown)
	}

	for name, r := range map[string]*pionstun.Message{
		"plain":          build(),
		"optional only":  build(0x8030),
		"known required": build(pionstun.AttrUsername),
	} {
		raw, err := Respond(r.Raw, src)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		m := &pionstun.Message{Raw: raw}
		var xor pionstun.XORMappedAddress
		if err := m.Decode(); err != nil || m.Type != pionstun.BindingSuccess || xor.GetFrom(m) != nil {
			t.Fatalf("%s: bad response %v %v", name, m, err)
		}
	}
}

func TestUnknownRequiredErrorUsesByteBudget(t *testing.T) {
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// A budget below one error response and almost no refill.
	s := New(slog.New(slog.NewJSONHandler(io.Discard, nil)), 1, 10)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, pc) }()
	defer func() { cancel(); <-done }()
	client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	req := pionstun.MustBuild(pionstun.TransactionID, pionstun.BindingRequest,
		pionstun.RawAttribute{Type: 0x0030, Value: []byte{1, 2, 3, 4}}, pionstun.Fingerprint)
	if _, err := client.WriteTo(req.Raw, pc.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, _, err := client.ReadFrom(make([]byte, 1500)); err == nil {
		t.Fatalf("error reply of %d bytes sent over the budget", n)
	}
	// The read timeout does not synchronize with the server's counter
	// updates, so wait for the completed outcome before asserting totals.
	if reqs, resps, limited := waitForOutcomes(s.Stats, 1, 5*time.Second); reqs != 1 || resps != 0 || limited != 1 {
		t.Fatalf("requests %d, responses %d, limited %d", reqs, resps, limited)
	}
}

// RFC 8489 section 9: attributes after MESSAGE-INTEGRITY (or after
// MESSAGE-INTEGRITY-SHA256 when MESSAGE-INTEGRITY is absent) are ignored,
// so an unknown required attribute there must not trigger a 420.
func TestUnknownRequiredAfterIntegrityBoundaryIgnored(t *testing.T) {
	src := &net.UDPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 5}
	unknownAttr := pionstun.RawAttribute{Type: 0x0030, Value: []byte{1, 2, 3, 4}}
	integrity := pionstun.RawAttribute{Type: pionstun.AttrMessageIntegrity, Value: make([]byte, 20)}
	integritySHA256 := pionstun.RawAttribute{Type: pionstun.AttrMessageIntegritySHA256, Value: make([]byte, 32)}

	for name, tc := range map[string]struct {
		attrs   []pionstun.Setter
		wantErr bool
	}{
		"after message-integrity":                 {[]pionstun.Setter{integrity, unknownAttr}, false},
		"after integrity and sha256":              {[]pionstun.Setter{integrity, integritySHA256, unknownAttr}, false},
		"after sha256 without message-integrity":  {[]pionstun.Setter{integritySHA256, unknownAttr}, false},
		"before message-integrity":                {[]pionstun.Setter{unknownAttr, integrity}, true},
		"between message-integrity and sha256":    {[]pionstun.Setter{integrity, unknownAttr, integritySHA256}, false},
		"before sha256 without message-integrity": {[]pionstun.Setter{unknownAttr, integritySHA256}, true},
	} {
		setters := append([]pionstun.Setter{pionstun.TransactionID, pionstun.BindingRequest}, tc.attrs...)
		setters = append(setters, pionstun.Fingerprint)
		req := pionstun.MustBuild(setters...)
		raw, err := Respond(req.Raw, src)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		m := &pionstun.Message{Raw: raw}
		if err := m.Decode(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := pionstun.BindingSuccess
		if tc.wantErr {
			want = pionstun.BindingError
		}
		if m.Type != want {
			t.Fatalf("%s: response type %v, want %v", name, m.Type, want)
		}
	}
}
