package datapath

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"sort"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

// BindingSaltSize is the size of the per-operation binding salt.
const BindingSaltSize = 32

// NewBindingSalt returns 32 random bytes for one operation.
func NewBindingSalt() []byte {
	b := make([]byte, BindingSaltSize)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return b
}

// ExecBinding builds the RelayRequestBinding of an exec request.
func ExecBinding(req *ownsandv1.ExecRequest) *ownsandv1.RelayRequestBinding {
	keys := make([]string, 0, len(req.GetEnv()))
	for k := range req.GetEnv() {
		keys = append(keys, k)
	}
	// Sort by key bytes. Go string comparison compares bytes.
	sort.Strings(keys)
	env := make([]*ownsandv1.RelayRequestBinding_EnvEntry, 0, len(keys))
	for _, k := range keys {
		env = append(env, &ownsandv1.RelayRequestBinding_EnvEntry{Key: k, Value: req.GetEnv()[k]})
	}
	var timeout *durationpb.Duration
	if req.GetTimeout() != nil {
		timeout = durationpb.New(req.GetTimeout().AsDuration())
	}
	return &ownsandv1.RelayRequestBinding{
		BindingSalt:      bytes.Clone(req.GetBindingSalt()),
		SandboxId:        req.GetSandboxId(),
		OperationKind:    ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC,
		OperationId:      req.GetExecId(),
		Command:          append([]string(nil), req.GetCommand()...),
		Env:              env,
		WorkingDirectory: req.GetWorkingDirectory(),
		Timeout:          timeout,
	}
}

// ReadBinding builds the RelayRequestBinding of a file read.
func ReadBinding(salt []byte, sandboxID, readID, path string) *ownsandv1.RelayRequestBinding {
	return &ownsandv1.RelayRequestBinding{
		BindingSalt:   bytes.Clone(salt),
		SandboxId:     sandboxID,
		OperationKind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_READ_FILE,
		OperationId:   readID,
		Path:          path,
	}
}

// WriteBinding builds the RelayRequestBinding of a file write.
func WriteBinding(salt []byte, sandboxID, writeID, path string) *ownsandv1.RelayRequestBinding {
	return &ownsandv1.RelayRequestBinding{
		BindingSalt:   bytes.Clone(salt),
		SandboxId:     sandboxID,
		OperationKind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_WRITE_FILE,
		OperationId:   writeID,
		Path:          path,
	}
}

// Digest returns the SHA-256 of the binary encoding of b. The Go encoder
// writes known fields in field-number order; the binding has no map fields
// and the deterministic option keeps the output stable. Unknown fields are
// dropped before encoding.
func Digest(b *ownsandv1.RelayRequestBinding) []byte {
	clean := proto.Clone(b).(*ownsandv1.RelayRequestBinding)
	clean.ProtoReflect().SetUnknown(nil)
	for _, e := range clean.GetEnv() {
		e.ProtoReflect().SetUnknown(nil)
	}
	if clean.GetTimeout() != nil {
		clean.GetTimeout().ProtoReflect().SetUnknown(nil)
	}
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(clean)
	if err != nil {
		panic(fmt.Sprintf("marshal RelayRequestBinding: %v", err))
	}
	sum := sha256.Sum256(data)
	return sum[:]
}

// ExecDigest returns the request digest of an exec request.
func ExecDigest(req *ownsandv1.ExecRequest) []byte {
	return Digest(ExecBinding(req))
}

// ReadDigest returns the request digest of a ReadFile request.
func ReadDigest(req *ownsandv1.ReadFileRequest) []byte {
	return Digest(ReadBinding(req.GetBindingSalt(), req.GetSandboxId(), req.GetReadId(), req.GetPath()))
}

// WriteDigest returns the request digest of the first WriteFile chunk.
func WriteDigest(req *ownsandv1.WriteFileRequest) []byte {
	return Digest(WriteBinding(req.GetBindingSalt(), req.GetSandboxId(), req.GetWriteId(), req.GetPath()))
}
