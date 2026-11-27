// SPDX-License-Identifier: FSL-1.1-ALv2

package update

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

type artifactCloseTransport struct {
	base               http.RoundTripper
	once               sync.Once
	closeFirstArtifact func()
}

func (tr *artifactCloseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := tr.base.RoundTrip(req)
	if err == nil && req.URL.Path == "/agent" {
		tr.once.Do(func() {
			resp.Body = &artifactCloseBody{ReadCloser: resp.Body, afterClose: tr.closeFirstArtifact}
		})
	}
	return resp, err
}

type artifactCloseBody struct {
	io.ReadCloser
	afterClose func()
}

func (b *artifactCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.afterClose()
	return err
}

type cancelAfterArtifactWrite struct {
	context.Context
	cancel context.CancelFunc
	path   string
}

// Err cancels at the first cancellation check after the completed file is
// visible. The file goes through the real atomic write.
func (c cancelAfterArtifactWrite) Err() error {
	if _, err := os.Stat(c.path); err == nil {
		c.cancel()
	}
	return c.Context.Err()
}

func TestUpdateChannelCancellationBeforePublication(t *testing.T) {
	tests := []struct {
		name       string
		running    string
		afterWrite bool
		wantState  ownsandv1.AgentUpdateState
	}{
		{"body close", "1.0.0", false, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED},
		{"after write", "1.0.0", true, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED},
		{"revoked running version", "0.9.0", false, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRelease(t, nil)
			u, inst, restarts := newUpdater(t, r, tt.running, &atomic.Bool{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var channelCtx context.Context = ctx
			dir := u.opts.Store.Path(store.UpdateDir)
			if tt.afterWrite {
				channelCtx = cancelAfterArtifactWrite{Context: ctx, cancel: cancel, path: filepath.Join(dir, "ownsand-agent-1.1.0-1")}
			} else {
				u.opts.HTTP = &http.Client{Transport: &artifactCloseTransport{
					base: r.srv.Client().Transport, closeFirstArtifact: cancel,
				}}
			}

			cmd := &ownsandv1.UpdateAgentCommand{TargetVersion: "1.1.0"}
			if err := u.Handle(channelCtx, cmd); !errors.Is(err, context.Canceled) {
				t.Fatalf("update error = %v, want context canceled", err)
			}
			if state, target := u.State(); state != tt.wantState || target != "1.1.0" {
				t.Fatalf("update state = %v %q, want %v 1.1.0", state, target, tt.wantState)
			}
			if tt.wantState == ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE {
				if _, err := u.BeginCreate(); !errors.Is(err, ErrRunningRevoked) {
					t.Fatalf("create admission = %v, want ErrRunningRevoked", err)
				}
			}
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 0 || u.artifact != "" {
				t.Fatalf("canceled update left files %v or queued artifact %q: %v", files, u.artifact, err)
			}
			select {
			case <-u.wake:
				t.Fatal("canceled update woke the installer")
			default:
			}
			if ok, err := u.TryInstall(); ok || err != nil || inst.installed.Load() != 0 || restarts.Load() != 0 {
				t.Fatalf("canceled update reached installer or restart: installed = %v, err = %v, calls = %d, restarts = %d",
					ok, err, inst.installed.Load(), restarts.Load())
			}
		})
	}
}

func TestUpdateChannelCancellationAfterPublication(t *testing.T) {
	r := newRelease(t, nil)
	u, inst, restarts := newUpdater(t, r, "1.0.0", &atomic.Bool{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := u.Handle(ctx, &ownsandv1.UpdateAgentCommand{TargetVersion: "1.1.0"}); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-u.wake:
	default:
		t.Fatal("published update did not wake the installer")
	}
	if ok, err := u.TryInstall(); !ok || err != nil || inst.installed.Load() != 1 || restarts.Load() != 1 {
		t.Fatalf("published update: installed = %v, err = %v, calls = %d, restarts = %d",
			ok, err, inst.installed.Load(), restarts.Load())
	}
}

func TestCanceledUpdatePreservesNewerQueuedArtifact(t *testing.T) {
	r := newRelease(t, nil)
	u, inst, restarts := newUpdater(t, r, "1.0.0", &atomic.Bool{})
	closing := make(chan struct{})
	resume := make(chan struct{})
	u.opts.HTTP = &http.Client{Transport: &artifactCloseTransport{
		base: r.srv.Client().Transport,
		closeFirstArtifact: func() {
			close(closing)
			<-resume
		},
	}}
	cmd := &ownsandv1.UpdateAgentCommand{TargetVersion: "1.1.0"}
	older, cancel := startUpdateWorker(t, u, cmd, u.NextOrder())
	finishClose := sync.OnceFunc(func() { close(resume) })
	t.Cleanup(finishClose)
	select {
	case <-closing:
	case err := <-older:
		t.Fatalf("older command finished before its artifact body closed: %v", err)
	case <-time.After(gateTimeout):
		t.Fatal("older command did not reach artifact body close")
	}
	cancel()
	if err := u.Handle(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	finishClose()
	if err := awaitDone(t, older); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("older command error = %v, want ErrSuperseded", err)
	}
	if state, target := u.State(); state != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_PENDING_IDLE || target != "1.1.0" {
		t.Fatalf("newer update state = %v %q", state, target)
	}
	files, err := os.ReadDir(u.opts.Store.Path(store.UpdateDir))
	if err != nil || len(files) != 1 || files[0].Name() != "ownsand-agent-1.1.0-2" {
		t.Fatalf("update directory = %v, want only the newer artifact: %v", files, err)
	}
	if ok, err := u.TryInstall(); !ok || err != nil || inst.installed.Load() != 1 || restarts.Load() != 1 {
		t.Fatalf("newer update: installed = %v, err = %v, calls = %d, restarts = %d",
			ok, err, inst.installed.Load(), restarts.Load())
	}
}
