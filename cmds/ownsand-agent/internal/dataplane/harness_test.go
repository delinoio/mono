// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package dataplane

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/ops"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/sandboxes"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/processrt"
	"github.com/delinoio/mono/go/ownsand/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

var guestBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ownsand-dataplane-test-")
	if err != nil {
		panic(err)
	}
	guestBinary = filepath.Join(dir, "ownsand-guest")
	if out, err := exec.Command("go", "build", "-o", guestBinary, "github.com/delinoio/mono/cmds/ownsand-guest").CombinedOutput(); err != nil {
		panic(fmt.Sprintf("build guest helper: %v\n%s", err, out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

const orgID = "0192f4b6-7c3a-7def-8abc-0123456789ab"

func logger() *slog.Logger {
	if os.Getenv("OWNSAND_TEST_VERBOSE") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// fakeChannel is an agent channel that records envelopes.
type fakeChannel struct {
	gen  uint64
	mu   sync.Mutex
	sent []*ownsandv1.AgentEnvelope
	done chan struct{}
	once sync.Once
}

func newChannel(gen uint64) *fakeChannel {
	return &fakeChannel{gen: gen, done: make(chan struct{})}
}

func (c *fakeChannel) Generation() uint64    { return c.gen }
func (c *fakeChannel) Done() <-chan struct{} { return c.done }
func (c *fakeChannel) Close()                { c.once.Do(func() { close(c.done) }) }
func (c *fakeChannel) Send(env *ownsandv1.AgentEnvelope) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, env)
	return true
}

func (c *fakeChannel) released(grantID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.sent {
		if e.GetRelayGrantRelease().GetGrantId() == grantID {
			return true
		}
	}
	return false
}

// world is a running sandbox with the dispatcher.
type world struct {
	t       *testing.T
	m       *sandboxes.Manager
	d       *ops.Dispatcher
	sandbox string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rt, err := processrt.New(guestBinary, logger())
	if err != nil {
		t.Fatal(err)
	}
	m, err := sandboxes.New(context.Background(), sandboxes.Options{Store: st, Runtime: rt, Logger: logger()})
	if err != nil {
		t.Fatal(err)
	}
	d, err := ops.New(m, st, ops.Options{Logger: logger()})
	if err != nil {
		t.Fatal(err)
	}
	m.SetExecTracker(d)
	t.Cleanup(func() {
		d.Close()
		m.Close()
		rt.Close()
	})
	w := &world{t: t, m: m, d: d, sandbox: newID(t)}
	now := time.Now()
	if _, err := m.Create(context.Background(), &ownsandv1.CreateSandboxCommand{
		SandboxId: w.sandbox, Source: &ownsandv1.CreateSandboxCommand_BaseImage{BaseImage: ownsandv1.BaseImage_BASE_IMAGE_LINUX},
		Resources: &ownsandv1.Resources{VcpuCount: 1, MemoryMib: 128, DiskGib: 1}, NetworkMode: ownsandv1.NetworkMode_NETWORK_MODE_NONE,
		GuestOs: ownsandv1.GuestOs_GUEST_OS_LINUX, IdleTtl: durationpb.New(time.Hour), MaxTtl: durationpb.New(time.Hour),
		CreateTime: timestamppb.New(now), MaxDeadline: timestamppb.New(now.Add(time.Hour)),
		Limits: &ownsandv1.DeviceLimits{VcpuCount: 4, MemoryMib: 4096, DiskGib: 4, VcpuOvercommitPercent: 100},
	}); err != nil {
		t.Fatal(err)
	}
	return w
}

func newID(t *testing.T) string {
	id, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (w *world) execReq(cmd ...string) *ownsandv1.ExecRequest {
	salt := make([]byte, 32)
	rand.Read(salt)
	return &ownsandv1.ExecRequest{OrganizationId: orgID, SandboxId: w.sandbox, ExecId: newID(w.t), Command: cmd, BindingSalt: salt}
}
