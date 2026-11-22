// SPDX-License-Identifier: FSL-1.1-ALv2

package stun

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	pionstun "github.com/pion/stun/v3"

	"github.com/delinoio/mono/servers/ownsand-api/internal/config"
)

// The smallest accepted burst must admit every response the server builds:
// the limiter drops a response larger than the whole burst.
func TestResponsesFitMinimumBurst(t *testing.T) {
	plain := pionstun.MustBuild(pionstun.TransactionID, pionstun.BindingRequest, pionstun.Fingerprint)
	unknown := pionstun.MustBuild(pionstun.TransactionID, pionstun.BindingRequest,
		pionstun.RawAttribute{Type: 0x0030, Value: []byte{1, 2, 3, 4}}, pionstun.Fingerprint)
	for name, tc := range map[string]struct {
		req *pionstun.Message
		src *net.UDPAddr
	}{
		"ipv4 success": {plain, &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 4000}},
		"ipv6 success": {plain, &net.UDPAddr{IP: net.ParseIP("2001:db8::1"), Port: 4000}},
		"ipv4 420":     {unknown, &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 4000}},
		"ipv6 420":     {unknown, &net.UDPAddr{IP: net.ParseIP("2001:db8::1"), Port: 4000}},
	} {
		resp, err := Respond(tc.req.Raw, tc.src)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(resp) > config.MinSTUNRateBurstBytes {
			t.Errorf("%s: response of %d bytes exceeds the %d-byte minimum burst", name, len(resp), config.MinSTUNRateBurstBytes)
		}
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

func TestRateLimitPerSource(t *testing.T) {
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// A budget of about two responses and almost no refill.
	s := New(slog.New(slog.NewJSONHandler(io.Discard, nil)), 1, 100)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, pc) }()
	defer func() { cancel(); <-done }()
	client, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer client.Close()
	for i := 0; i < 10; i++ {
		req := pionstun.MustBuild(pionstun.TransactionID, pionstun.BindingRequest)
		_, _ = client.WriteTo(req.Raw, pc.LocalAddr())
	}
	req, resp, limited := waitForOutcomes(s.Stats, 10, 5*time.Second)
	if req != 10 || resp == 0 || resp > 3 || limited != req-resp {
		t.Fatalf("requests %d, responses %d, limited %d", req, resp, limited)
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
	if reqs, resps, limited := s.Stats(); reqs != 1 || resps != 0 || limited != 1 {
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
