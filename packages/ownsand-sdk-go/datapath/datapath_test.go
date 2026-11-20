package datapath

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

var update = flag.Bool("update", false, "rewrite testdata/digest_vectors.json")

func TestUUIDv7(t *testing.T) {
	now := time.UnixMilli(1_790_000_000_123)
	u := NewUUIDv7At(now)
	if u.Version() != 7 {
		t.Fatalf("version = %d", u.Version())
	}
	if !u.Time().Equal(now) {
		t.Fatalf("time = %v, want %v", u.Time(), now)
	}
	p, err := ParseUUIDv7(u.String())
	if err != nil || p != u {
		t.Fatalf("round trip: %v %v", p, err)
	}
	if IsUUIDv7("00000000-0000-4000-8000-000000000000") {
		t.Fatal("v4 accepted as v7")
	}
	for _, bad := range []string{"", "not-a-uuid", "0190000000000000700080000000000000000"} {
		if _, err := ParseUUID(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestClassifyOperationID(t *testing.T) {
	now := time.Now()
	cases := []struct {
		at   time.Time
		want OperationIDAge
	}{
		{now, OperationIDFresh},
		{now.Add(-14 * time.Minute), OperationIDFresh},
		{now.Add(-15 * time.Minute), OperationIDExpired},
		{now.Add(-time.Hour), OperationIDExpired},
		{now.Add(5 * time.Minute), OperationIDFresh},
		{now.Add(5*time.Minute + time.Second), OperationIDFuture},
	}
	for _, c := range cases {
		if got := ClassifyOperationID(NewUUIDv7At(c.at), now); got != c.want {
			t.Errorf("at %v: got %v want %v", c.at.Sub(now), got, c.want)
		}
	}
}

type digestVector struct {
	Name       string `json:"name"`
	BindingHex string `json:"binding_hex"`
	DigestHex  string `json:"digest_hex"`
}

func vectorBindings() map[string]*ownsandv1.RelayRequestBinding {
	salt := bytes.Repeat([]byte{0xab}, BindingSaltSize)
	sandbox := "0190f5d6-1c2b-7a3e-8f00-000000000001"
	return map[string]*ownsandv1.RelayRequestBinding{
		"exec": ExecBinding(&ownsandv1.ExecRequest{
			SandboxId:        sandbox,
			ExecId:           "0190f5d6-1c2b-7a3e-8f00-000000000002",
			Command:          []string{"/bin/sh", "-c", "echo héllo"},
			Env:              map[string]string{"Z": "last", "A": "first", "a": "lower", "É": "utf8"},
			WorkingDirectory: "/work",
			Timeout:          durationpb.New(90*time.Second + 5*time.Millisecond),
			BindingSalt:      salt,
		}),
		"exec_minimal": ExecBinding(&ownsandv1.ExecRequest{
			SandboxId:   sandbox,
			ExecId:      "0190f5d6-1c2b-7a3e-8f00-000000000003",
			Command:     []string{"true"},
			BindingSalt: salt,
		}),
		"read":  ReadBinding(salt, sandbox, "0190f5d6-1c2b-7a3e-8f00-000000000004", "/etc/hostname"),
		"write": WriteBinding(salt, sandbox, "0190f5d6-1c2b-7a3e-8f00-000000000005", "C:\\Users\\a.txt"),
	}
}

// TestDigestVectors pins the digest of fixed bindings. The TypeScript SDK
// reads the same file and must produce the same digests.
func TestDigestVectors(t *testing.T) {
	names := []string{"exec", "exec_minimal", "read", "write"}
	bindings := vectorBindings()
	var got []digestVector
	for _, n := range names {
		b := bindings[n]
		enc, err := proto.MarshalOptions{Deterministic: true}.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, digestVector{Name: n, BindingHex: hex.EncodeToString(enc), DigestHex: hex.EncodeToString(Digest(b))})
	}
	path := filepath.Join("testdata", "digest_vectors.json")
	if *update {
		data, _ := json.MarshalIndent(got, "", "  ")
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want []digestVector
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	if len(want) != len(got) {
		t.Fatalf("vector count %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("vector %s: got %+v want %+v", want[i].Name, got[i], want[i])
		}
	}
}

func TestDigestIgnoresMapOrderAndUnknownFields(t *testing.T) {
	req := &ownsandv1.ExecRequest{SandboxId: "s", ExecId: "e", Command: []string{"x"}, Env: map[string]string{"b": "2", "a": "1", "c": "3"}, BindingSalt: []byte{1}}
	d1 := ExecDigest(req)
	for range 20 {
		if !bytes.Equal(d1, ExecDigest(proto.Clone(req).(*ownsandv1.ExecRequest))) {
			t.Fatal("digest depends on map order")
		}
	}
	b := ExecBinding(req)
	b.ProtoReflect().SetUnknown([]byte{0xf8, 0x07, 0x01})
	if !bytes.Equal(d1, Digest(b)) {
		t.Fatal("unknown fields changed the digest")
	}
	req2 := proto.Clone(req).(*ownsandv1.ExecRequest)
	req2.BindingSalt = []byte{2}
	if bytes.Equal(d1, ExecDigest(req2)) {
		t.Fatal("salt does not change the digest")
	}
}

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewFrameWriter(&buf)
	frames := []*ownsandv1.DataFrame{
		NewFrame(1, "s", func(f *ownsandv1.DataFrame) {
			f.Payload = &ownsandv1.DataFrame_Keepalive{Keepalive: &ownsandv1.FrameKeepalive{EchoRequested: true}}
		}),
		NewFrame(2, "s", func(f *ownsandv1.DataFrame) {
			f.Payload = &ownsandv1.DataFrame_FileChunk{FileChunk: &ownsandv1.FileChunk{Offset: 7, Data: bytes.Repeat([]byte{1}, MaxFileChunk)}}
		}),
	}
	for _, f := range frames {
		if err := w.Write(f); err != nil {
			t.Fatal(err)
		}
	}
	// Reassemble from data channel sized messages.
	var re bytes.Buffer
	for _, m := range SplitMessages(buf.Bytes()) {
		if len(m) > DataChannelMessageSize {
			t.Fatalf("message of %d bytes", len(m))
		}
		re.Write(m)
	}
	r := NewFrameReader(&re)
	for _, want := range frames {
		got, err := r.ReadDataFrame()
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(got, want) {
			t.Fatal("frame mismatch")
		}
	}
	if _, err := r.ReadDataFrame(); !errors.Is(err, io.EOF) {
		t.Fatalf("want EOF, got %v", err)
	}
	if frames[1].GetKind() != ownsandv1.DataFrameKind_DATA_FRAME_KIND_FILE_CHUNK || !PayloadUsesCredit(frames[1]) || PayloadUsesCredit(frames[0]) {
		t.Fatal("kind or credit classification")
	}
}

func TestFrameReaderRejectsOversizedLength(t *testing.T) {
	var b []byte
	b = append(b, 0xff, 0xff, 0xff, 0x7f) // a large varint
	r := NewFrameReader(bytes.NewReader(b))
	if _, err := r.ReadRaw(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("got %v", err)
	}
	trunc := []byte{0x05, 0x01}
	if _, err := NewFrameReader(bytes.NewReader(trunc)).ReadRaw(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("got %v", err)
	}
}

func TestSupportedKindsCoverEveryPayload(t *testing.T) {
	kinds := map[ownsandv1.DataFrameKind]bool{}
	for _, k := range SupportedKinds() {
		kinds[k] = true
	}
	for v := range ownsandv1.DataFrameKind_name {
		k := ownsandv1.DataFrameKind(v)
		if k != ownsandv1.DataFrameKind_DATA_FRAME_KIND_UNSPECIFIED && !kinds[k] {
			t.Errorf("kind %v not in SupportedKinds", k)
		}
	}
}
