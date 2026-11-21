// SPDX-License-Identifier: Apache-2.0

// Package relaybinding builds the RelayRequestBinding of an exec, file read
// or file write request and computes its request digest, the SHA-256 that
// binds a relay grant to the request. The caller computes the digest before
// AuthorizeRelay; the agent computes it again from the first request frame.
// The owning contract is the relay grants section of
// docs/protos-ownsand-api-contract.md.
//
// The digest input is the protobuf binary encoding of the binding with
// fields in field-number order and no unknown fields. Encode writes that
// encoding explicitly with protowire, so it does not depend on the field
// order of a protobuf runtime. testdata/request-digest-vectors.json holds
// golden vectors that the TypeScript SDK reuses.
package relaybinding

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// SaltSize is the size of a binding salt: 32 random bytes per operation.
const SaltSize = 32

// DigestSize is the size of a request digest.
const DigestSize = sha256.Size

// Field numbers of RelayRequestBinding, EnvironmentEntry and
// google.protobuf.Duration.
const (
	fieldBindingSalt      protowire.Number = 1
	fieldSandboxID        protowire.Number = 2
	fieldOperationKind    protowire.Number = 3
	fieldOperationID      protowire.Number = 4
	fieldCommand          protowire.Number = 5
	fieldEnv              protowire.Number = 6
	fieldWorkingDirectory protowire.Number = 7
	fieldTimeout          protowire.Number = 8
	fieldPath             protowire.Number = 9

	fieldEntryKey   protowire.Number = 1
	fieldEntryValue protowire.Number = 2

	fieldDurationSeconds protowire.Number = 1
	fieldDurationNanos   protowire.Number = 2
)

// ErrNoDigest reports an operation kind that has no request digest
// (ATTACH_EXEC) or no operation kind.
var ErrNoDigest = errors.New("relaybinding: operation kind has no request digest")

// NewSalt returns SaltSize random bytes from crypto/rand.
func NewSalt() ([]byte, error) {
	salt := make([]byte, SaltSize)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("relaybinding: read random salt: %w", err)
	}
	return salt, nil
}

// SortedEnv returns the entries of env sorted by key bytes.
func SortedEnv(env map[string]string) []*ownsandv1.EnvironmentEntry {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	// Go compares strings by their bytes, which is the order the contract
	// requires (not UTF-16 code unit order).
	slices.SortFunc(keys, strings.Compare)
	entries := make([]*ownsandv1.EnvironmentEntry, 0, len(keys))
	for _, key := range keys {
		entries = append(entries, &ownsandv1.EnvironmentEntry{Key: key, Value: env[key]})
	}
	return entries
}

// ForExec returns the binding of an exec request.
func ForExec(req *ownsandv1.ExecRequest) *ownsandv1.RelayRequestBinding {
	return &ownsandv1.RelayRequestBinding{
		BindingSalt:      req.GetBindingSalt(),
		SandboxId:        req.GetSandboxId(),
		OperationKind:    ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC,
		OperationId:      req.GetExecId(),
		Command:          req.GetCommand(),
		Env:              SortedEnv(req.GetEnv()),
		WorkingDirectory: req.GetWorkingDirectory(),
		Timeout:          req.GetTimeout(),
	}
}

// ForReadFile returns the binding of a file read request. Every ReadFile
// call carries the path and the salt.
func ForReadFile(req *ownsandv1.ReadFileRequest) *ownsandv1.RelayRequestBinding {
	return &ownsandv1.RelayRequestBinding{
		BindingSalt:   req.GetBindingSalt(),
		SandboxId:     req.GetSandboxId(),
		OperationKind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_READ_FILE,
		OperationId:   req.GetReadId(),
		Path:          req.GetPath(),
	}
}

// ForWriteFile returns the binding of the first WriteFile call of a
// write_id, which carries the path and the salt.
func ForWriteFile(req *ownsandv1.WriteFileRequest) *ownsandv1.RelayRequestBinding {
	return &ownsandv1.RelayRequestBinding{
		BindingSalt:   req.GetBindingSalt(),
		SandboxId:     req.GetSandboxId(),
		OperationKind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_WRITE_FILE,
		OperationId:   req.GetWriteId(),
		Path:          req.GetPath(),
	}
}

// Encode returns the protobuf binary encoding of b with fields in
// field-number order. Proto3 scalar fields with their zero value are
// omitted, repeated fields keep every element (also empty strings), and a
// present timeout is encoded even when it is zero. Unknown fields of b are
// ignored.
func Encode(b *ownsandv1.RelayRequestBinding) []byte {
	var out []byte
	out = appendBytes(out, fieldBindingSalt, b.GetBindingSalt())
	out = appendString(out, fieldSandboxID, b.GetSandboxId())
	if kind := b.GetOperationKind(); kind != 0 {
		out = protowire.AppendTag(out, fieldOperationKind, protowire.VarintType)
		// Enum values are encoded as int32 varints (sign-extended).
		out = protowire.AppendVarint(out, uint64(int64(kind)))
	}
	out = appendString(out, fieldOperationID, b.GetOperationId())
	for _, arg := range b.GetCommand() {
		out = protowire.AppendTag(out, fieldCommand, protowire.BytesType)
		out = protowire.AppendString(out, arg)
	}
	for _, entry := range b.GetEnv() {
		var e []byte
		e = appendString(e, fieldEntryKey, entry.GetKey())
		e = appendString(e, fieldEntryValue, entry.GetValue())
		out = protowire.AppendTag(out, fieldEnv, protowire.BytesType)
		out = protowire.AppendBytes(out, e)
	}
	out = appendString(out, fieldWorkingDirectory, b.GetWorkingDirectory())
	if timeout := b.GetTimeout(); timeout != nil {
		var d []byte
		if s := timeout.GetSeconds(); s != 0 {
			d = protowire.AppendTag(d, fieldDurationSeconds, protowire.VarintType)
			d = protowire.AppendVarint(d, uint64(s))
		}
		if n := timeout.GetNanos(); n != 0 {
			d = protowire.AppendTag(d, fieldDurationNanos, protowire.VarintType)
			d = protowire.AppendVarint(d, uint64(int64(n)))
		}
		out = protowire.AppendTag(out, fieldTimeout, protowire.BytesType)
		out = protowire.AppendBytes(out, d)
	}
	out = appendString(out, fieldPath, b.GetPath())
	return out
}

// Digest returns the SHA-256 of Encode(b).
func Digest(b *ownsandv1.RelayRequestBinding) []byte {
	sum := sha256.Sum256(Encode(b))
	return sum[:]
}

// ExecDigest returns the request digest of an exec request.
func ExecDigest(req *ownsandv1.ExecRequest) []byte {
	return Digest(ForExec(req))
}

// ReadFileDigest returns the request digest of a file read request.
func ReadFileDigest(req *ownsandv1.ReadFileRequest) []byte {
	return Digest(ForReadFile(req))
}

// WriteFileDigest returns the request digest of the first WriteFile call of
// a write_id.
func WriteFileDigest(req *ownsandv1.WriteFileRequest) []byte {
	return Digest(ForWriteFile(req))
}

// DigestOfFirstRequest returns the request digest of the first request
// frame of a relay stream, for a grant of kind. An exec attach request
// (allowed under EXEC and ATTACH_EXEC grants) and a WriteFile chunk after the
// first carry no bound fields and give ErrNoDigest; the agent then compares
// the grant digest with the digest it recorded for the operation.
func DigestOfFirstRequest(frame *ownsandv1.DataFrame) ([]byte, error) {
	switch p := frame.GetPayload().(type) {
	case *ownsandv1.DataFrame_ExecRequest:
		return ExecDigest(p.ExecRequest), nil
	case *ownsandv1.DataFrame_FileReadRequest:
		return ReadFileDigest(p.FileReadRequest), nil
	case *ownsandv1.DataFrame_FileWriteChunk:
		if p.FileWriteChunk.GetPath() == "" && len(p.FileWriteChunk.GetBindingSalt()) == 0 {
			return nil, ErrNoDigest
		}
		return WriteFileDigest(p.FileWriteChunk), nil
	}
	return nil, ErrNoDigest
}

// Equal reports whether a and b are the same request digest. It compares
// in constant time with respect to their content and rejects values that
// are not DigestSize bytes long.
func Equal(a, b []byte) bool {
	if len(a) != DigestSize || len(b) != DigestSize {
		return false
	}
	return subtle.ConstantTimeCompare(a, b) == 1
}

func appendString(out []byte, field protowire.Number, value string) []byte {
	if value == "" {
		return out
	}
	out = protowire.AppendTag(out, field, protowire.BytesType)
	return protowire.AppendString(out, value)
}

func appendBytes(out []byte, field protowire.Number, value []byte) []byte {
	if len(value) == 0 {
		return out
	}
	out = protowire.AppendTag(out, field, protowire.BytesType)
	return protowire.AppendBytes(out, value)
}
