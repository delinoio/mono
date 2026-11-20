// SPDX-License-Identifier: Apache-2.0

package relaybinding

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

var update = flag.Bool("update", false, "rewrite testdata/request-digest-vectors.json")

const vectorsPath = "testdata/request-digest-vectors.json"

// vectorFile is the language-neutral format of the golden vectors. The
// TypeScript SDK reads the same file.
type vectorFile struct {
	Description string   `json:"description"`
	Vectors     []vector `json:"vectors"`
}

type envEntry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type durationJSON struct {
	// Seconds is a decimal string, like the protobuf JSON mapping of int64.
	Seconds string `json:"seconds"`
	Nanos   int32  `json:"nanos"`
}

type requestJSON struct {
	// Kind is `exec`, `read_file` or `write_file`.
	Kind           string `json:"kind"`
	BindingSaltHex string `json:"bindingSaltHex"`
	SandboxID      string `json:"sandboxId"`
	OperationID    string `json:"operationId"`
	// Exec only. Env lists the entries in an arbitrary order; an
	// implementation puts them in a map and sorts them by key bytes.
	Command          []string      `json:"command,omitempty"`
	Env              []envEntry    `json:"env,omitempty"`
	WorkingDirectory string        `json:"workingDirectory,omitempty"`
	Timeout          *durationJSON `json:"timeout,omitempty"`
	// Read and write only.
	Path string `json:"path,omitempty"`
}

type vector struct {
	Name               string      `json:"name"`
	Request            requestJSON `json:"request"`
	EncodedBindingHex  string      `json:"encodedBindingHex"`
	RequestDigestHex   string      `json:"requestDigestHex"`
	SortedEnvKeysOrder []string    `json:"sortedEnvKeys,omitempty"`
}

func salt(start byte) string {
	b := make([]byte, SaltSize)
	for i := range b {
		b[i] = start + byte(i)
	}
	return hex.EncodeToString(b)
}

// vectorInputs are the inputs of the golden vectors.
func vectorInputs() []vector {
	const sandbox = "0192f4b6-7c3a-7def-8abc-0123456789ab"
	return []vector{
		{Name: "exec-minimal", Request: requestJSON{
			Kind: "exec", BindingSaltHex: salt(0), SandboxID: sandbox,
			OperationID: "0192f4b6-8000-7000-8000-000000000001",
			Command:     []string{"/bin/true"},
		}},
		{Name: "exec-full", Request: requestJSON{
			Kind: "exec", BindingSaltHex: salt(0x20), SandboxID: sandbox,
			OperationID: "0192f4b6-8000-7000-8000-000000000002",
			Command:     []string{"/bin/sh", "-c", "echo \"$GREETING\" > /tmp/out"},
			Env: []envEntry{
				{"PATH", "/usr/local/bin:/usr/bin:/bin"},
				{"GREETING", "hello, world"},
				{"Z_EMPTY", ""},
				{"A", "1"},
			},
			WorkingDirectory: "/workspace",
			Timeout:          &durationJSON{Seconds: "90", Nanos: 500_000_000},
		}},
		{Name: "exec-env-key-byte-order", Request: requestJSON{
			Kind: "exec", BindingSaltHex: salt(0x40), SandboxID: sandbox,
			OperationID: "0192f4b6-8000-7000-8000-000000000003",
			Command:     []string{"env"},
			// Byte order differs from UTF-16 code unit order for the last two
			// keys: U+FF21 (EF BC A1) sorts before U+1F600 (F0 9F 98 80) by
			// bytes, but after it (FF21 > D83D) by UTF-16 code units.
			Env: []envEntry{
				{"\U0001F600", "emoji"},
				{"\uFF21", "fullwidth"},
				{"\u00e9", "e-acute"},
				{"a", "lower"},
				{"_", "underscore"},
				{"B", "upper"},
			},
		}},
		{Name: "exec-empty-arguments-and-zero-timeout", Request: requestJSON{
			Kind: "exec", BindingSaltHex: salt(0x60), SandboxID: sandbox,
			OperationID: "0192f4b6-8000-7000-8000-000000000004",
			Command:     []string{"printf", "", "%s", ""},
			Timeout:     &durationJSON{Seconds: "0", Nanos: 0},
		}},
		{Name: "exec-long-argument-and-nanos-timeout", Request: requestJSON{
			Kind: "exec", BindingSaltHex: salt(0x80), SandboxID: sandbox,
			OperationID: "0192f4b6-8000-7000-8000-000000000005",
			Command:     []string{"echo", strings.Repeat("x", 300), "\u65e5\u672c\u8a9e"},
			Timeout:     &durationJSON{Seconds: "0", Nanos: 1},
		}},
		{Name: "exec-large-timeout", Request: requestJSON{
			Kind: "exec", BindingSaltHex: salt(0xa0), SandboxID: sandbox,
			OperationID: "0192f4b6-8000-7000-8000-000000000006",
			Command:     []string{"sleep", "infinity"},
			Timeout:     &durationJSON{Seconds: "604800", Nanos: 999_999_999},
		}},
		{Name: "read-file", Request: requestJSON{
			Kind: "read_file", BindingSaltHex: salt(0xc0), SandboxID: sandbox,
			OperationID: "0192f4b6-8000-7000-8000-000000000007",
			Path:        "/etc/hosts",
		}},
		{Name: "write-file", Request: requestJSON{
			Kind: "write_file", BindingSaltHex: salt(0xe0), SandboxID: sandbox,
			OperationID: "0192f4b6-8000-7000-8000-000000000008",
			Path:        "/home/user/My File \u00fc.txt",
		}},
		{Name: "write-file-windows-path", Request: requestJSON{
			Kind: "write_file", BindingSaltHex: salt(0x10), SandboxID: sandbox,
			OperationID: "0192f4b6-8000-7000-8000-000000000009",
			Path:        "C:\\Users\\ownsand\\out.bin",
		}},
	}
}

// bindingOf builds the binding of a vector request through the request
// messages, as the SDK and the agent do.
func bindingOf(t testing.TB, r requestJSON) *ownsandv1.RelayRequestBinding {
	t.Helper()
	saltBytes, err := hex.DecodeString(r.BindingSaltHex)
	if err != nil {
		t.Fatal(err)
	}
	switch r.Kind {
	case "exec":
		req := &ownsandv1.ExecRequest{
			SandboxId: r.SandboxID, ExecId: r.OperationID, Command: r.Command,
			WorkingDirectory: r.WorkingDirectory, BindingSalt: saltBytes,
		}
		if len(r.Env) > 0 {
			req.Env = map[string]string{}
			for _, e := range r.Env {
				req.Env[e.Key] = e.Value
			}
		}
		if r.Timeout != nil {
			var seconds int64
			if err := json.Unmarshal([]byte(r.Timeout.Seconds), &seconds); err != nil {
				t.Fatal(err)
			}
			req.Timeout = &durationpb.Duration{Seconds: seconds, Nanos: r.Timeout.Nanos}
		}
		return ForExec(req)
	case "read_file":
		return ForReadFile(&ownsandv1.ReadFileRequest{SandboxId: r.SandboxID, ReadId: r.OperationID, Path: r.Path, BindingSalt: saltBytes, Offset: 12345})
	case "write_file":
		return ForWriteFile(&ownsandv1.WriteFileRequest{SandboxId: r.SandboxID, WriteId: r.OperationID, Path: r.Path, BindingSalt: saltBytes, Offset: 99, Data: []byte("ignored")})
	}
	t.Fatalf("unknown kind %q", r.Kind)
	return nil
}

func computeVectors(t testing.TB) vectorFile {
	file := vectorFile{
		Description: "Golden vectors of the OwnSand relay request digest: request_digest = SHA-256 of the protobuf encoding of delino.ownsand.v1.RelayRequestBinding with fields in field-number order. Env entries are listed in an arbitrary order and must be sorted by key bytes. Generated by packages/ownsand-sdk-go/relaybinding tests with -update.",
	}
	for _, v := range vectorInputs() {
		b := bindingOf(t, v.Request)
		encoded := Encode(b)
		v.EncodedBindingHex = hex.EncodeToString(encoded)
		v.RequestDigestHex = hex.EncodeToString(Digest(b))
		for _, e := range b.GetEnv() {
			v.SortedEnvKeysOrder = append(v.SortedEnvKeysOrder, e.GetKey())
		}
		file.Vectors = append(file.Vectors, v)
	}
	return file
}

func TestGoldenVectors(t *testing.T) {
	want := computeVectors(t)
	if *update {
		data, err := json.MarshalIndent(want, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(vectorsPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(vectorsPath, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	var got vectorFile
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Vectors) != len(want.Vectors) {
		t.Fatalf("file has %d vectors, want %d; run go test -update", len(got.Vectors), len(want.Vectors))
	}
	for i, v := range got.Vectors {
		// Recompute from the file's own inputs, so that the file stays
		// self-consistent for other implementations.
		b := bindingOf(t, v.Request)
		if hex.EncodeToString(Encode(b)) != v.EncodedBindingHex {
			t.Errorf("%s: encoding differs from the file", v.Name)
		}
		if hex.EncodeToString(Digest(b)) != v.RequestDigestHex {
			t.Errorf("%s: digest differs from the file", v.Name)
		}
		if v.EncodedBindingHex != want.Vectors[i].EncodedBindingHex {
			t.Errorf("%s: file differs from the inputs in the test; run go test -update", v.Name)
		}
	}
}

func TestEnvKeysSortedByBytes(t *testing.T) {
	for _, v := range computeVectors(t).Vectors {
		if v.Name != "exec-env-key-byte-order" {
			continue
		}
		want := []string{"B", "_", "a", "\u00e9", "\uFF21", "\U0001F600"}
		if strings.Join(v.SortedEnvKeysOrder, "|") != strings.Join(want, "|") {
			t.Fatalf("order %q, want %q", v.SortedEnvKeysOrder, want)
		}
		return
	}
	t.Fatal("vector not found")
}

// The explicit encoding equals the encoding of the protobuf runtime, which
// also writes fields in field-number order.
func TestEncodeMatchesProtobufRuntime(t *testing.T) {
	check := func(name string, b *ownsandv1.RelayRequestBinding) {
		t.Helper()
		runtime, err := proto.MarshalOptions{Deterministic: true}.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(Encode(b), runtime) {
			t.Fatalf("%s: explicit %x\nruntime  %x", name, Encode(b), runtime)
		}
		var decoded ownsandv1.RelayRequestBinding
		if err := proto.Unmarshal(Encode(b), &decoded); err != nil || !proto.Equal(&decoded, b) {
			t.Fatalf("%s: round trip failed: %v", name, err)
		}
	}
	for _, v := range vectorInputs() {
		check(v.Name, bindingOf(t, v.Request))
	}
	rng := rand.New(rand.NewPCG(5, 6))
	for i := 0; i < 2000; i++ {
		b := &ownsandv1.RelayRequestBinding{
			BindingSalt:      randomBytes(rng, rng.IntN(40)),
			SandboxId:        randomString(rng),
			OperationKind:    ownsandv1.RelayOperationKind(rng.IntN(6)),
			OperationId:      randomString(rng),
			WorkingDirectory: randomString(rng),
			Path:             randomString(rng),
		}
		for j := rng.IntN(4); j > 0; j-- {
			b.Command = append(b.Command, randomString(rng))
		}
		env := map[string]string{}
		for j := rng.IntN(4); j > 0; j-- {
			env[randomString(rng)] = randomString(rng)
		}
		b.Env = SortedEnv(env)
		if rng.IntN(2) == 0 {
			b.Timeout = &durationpb.Duration{Seconds: rng.Int64N(1 << 40), Nanos: rng.Int32N(1_000_000_000)}
		}
		check("random", b)
	}
}

func randomBytes(rng *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rng.Uint32())
	}
	return b
}

func randomString(rng *rand.Rand) string {
	alphabet := []rune("aZ_/ é日😀")
	n := rng.IntN(8)
	r := make([]rune, n)
	for i := range r {
		r[i] = alphabet[rng.IntN(len(alphabet))]
	}
	return string(r)
}

func TestDigestBindsEveryField(t *testing.T) {
	base := &ownsandv1.ExecRequest{
		SandboxId: "s", ExecId: "e", Command: []string{"a", "b"},
		Env: map[string]string{"K": "V"}, WorkingDirectory: "/w",
		Timeout: durationpb.New(0), BindingSalt: make([]byte, SaltSize),
	}
	digest := ExecDigest(base)
	variants := []func(r *ownsandv1.ExecRequest){
		func(r *ownsandv1.ExecRequest) { r.SandboxId = "t" },
		func(r *ownsandv1.ExecRequest) { r.ExecId = "f" },
		func(r *ownsandv1.ExecRequest) { r.Command = []string{"a b"} },
		func(r *ownsandv1.ExecRequest) { r.Command = []string{"a", "b", ""} },
		func(r *ownsandv1.ExecRequest) { r.Env = map[string]string{"K": "W"} },
		func(r *ownsandv1.ExecRequest) { r.Env = nil },
		func(r *ownsandv1.ExecRequest) { r.WorkingDirectory = "/x" },
		func(r *ownsandv1.ExecRequest) { r.Timeout = nil },
		func(r *ownsandv1.ExecRequest) { r.BindingSalt[0] = 1 },
	}
	for i, change := range variants {
		r := proto.Clone(base).(*ownsandv1.ExecRequest)
		change(r)
		if Equal(ExecDigest(r), digest) {
			t.Errorf("variant %d does not change the digest", i)
		}
	}
	// Fields outside the binding do not change it.
	r := proto.Clone(base).(*ownsandv1.ExecRequest)
	r.OrganizationId = "other"
	if !Equal(ExecDigest(r), digest) {
		t.Error("organization_id changed the digest")
	}
	// Read and write bindings of the same path differ by operation kind.
	read := ReadFileDigest(&ownsandv1.ReadFileRequest{SandboxId: "s", ReadId: "e", Path: "/p"})
	write := WriteFileDigest(&ownsandv1.WriteFileRequest{SandboxId: "s", WriteId: "e", Path: "/p"})
	if Equal(read, write) {
		t.Error("read and write digests are equal")
	}
}

func TestDigestOfFirstRequest(t *testing.T) {
	exec := &ownsandv1.ExecRequest{SandboxId: "s", ExecId: "e", Command: []string{"x"}}
	got, err := DigestOfFirstRequest(datapath.MustNewFrame(1, "s", exec))
	if err != nil || !Equal(got, ExecDigest(exec)) {
		t.Fatalf("exec: %v", err)
	}
	read := &ownsandv1.ReadFileRequest{SandboxId: "s", ReadId: "r", Path: "/p", BindingSalt: []byte{1}}
	if got, err := DigestOfFirstRequest(datapath.MustNewFrame(1, "s", read)); err != nil || !Equal(got, ReadFileDigest(read)) {
		t.Fatalf("read: %v", err)
	}
	first := &ownsandv1.WriteFileRequest{SandboxId: "s", WriteId: "w", Path: "/p", BindingSalt: []byte{1}}
	if got, err := DigestOfFirstRequest(datapath.MustNewFrame(1, "s", first)); err != nil || !Equal(got, WriteFileDigest(first)) {
		t.Fatalf("write: %v", err)
	}
	later := &ownsandv1.WriteFileRequest{SandboxId: "s", WriteId: "w", Offset: 10, Data: []byte("x")}
	if _, err := DigestOfFirstRequest(datapath.MustNewFrame(1, "s", later)); !errors.Is(err, ErrNoDigest) {
		t.Fatalf("later chunk: %v", err)
	}
	attach := &ownsandv1.AttachExecRequest{SandboxId: "s", ExecId: "e"}
	if _, err := DigestOfFirstRequest(datapath.MustNewFrame(1, "s", attach)); !errors.Is(err, ErrNoDigest) {
		t.Fatalf("attach: %v", err)
	}
}

func TestNewSaltAndEqual(t *testing.T) {
	a, err := NewSalt()
	if err != nil || len(a) != SaltSize {
		t.Fatalf("salt %d %v", len(a), err)
	}
	b, _ := NewSalt()
	if bytes.Equal(a, b) {
		t.Fatal("two salts are equal")
	}
	d := make([]byte, DigestSize)
	if !Equal(d, d) || Equal(d, d[:31]) || Equal(nil, nil) {
		t.Fatal("Equal")
	}
}
