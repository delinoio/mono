// SPDX-License-Identifier: FSL-1.1-ALv2

package github

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/delinoio/mono/go/ownsand"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/githubapi"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient"
	"github.com/delinoio/mono/servers/sandrun-api/internal/work"
)

type prepareExecClient struct {
	ownsandclient.Client
	exec func(context.Context, ownsand.ExecParams) (*ownsand.ExecResult, error)
}

func (c prepareExecClient) Exec(ctx context.Context, _ string, p ownsand.ExecParams) (*ownsand.ExecResult, error) {
	return c.exec(ctx, p)
}

func TestPrepareResult(t *testing.T) {
	zero, nonzero, signal := int64(0), int64(1), int32(15)
	transportErr := errors.New("connection lost")
	const timedOut = "the GitHub Actions runner installation timed out"
	a := &Adapter{l: &Layer{gh: &githubapi.Client{}, timing: DefaultTiming()}}

	for _, tt := range []struct {
		name    string
		result  *ownsand.ExecResult
		execErr error
		wantErr string
	}{
		{name: "timed out with zero exit", result: &ownsand.ExecResult{Kind: ownsand.ExecResultExited, ExitCode: &zero, TimedOut: true}, wantErr: timedOut},
		{name: "timed out with nonzero exit", result: &ownsand.ExecResult{Kind: ownsand.ExecResultExited, ExitCode: &nonzero, TimedOut: true}, wantErr: timedOut},
		{name: "timed out without exit", result: &ownsand.ExecResult{Kind: ownsand.ExecResultFailed, TimedOut: true}, wantErr: timedOut},
		{name: "missing result", wantErr: "the GitHub Actions runner installation returned no exec result"},
		{name: "success", result: &ownsand.ExecResult{Kind: ownsand.ExecResultExited, ExitCode: &zero}},
		{name: "nonzero exit", result: &ownsand.ExecResult{Kind: ownsand.ExecResultExited, ExitCode: &nonzero}, wantErr: "the GitHub Actions runner installation failed (exited, exit code 1)"},
		{name: "signal", result: &ownsand.ExecResult{Kind: ownsand.ExecResultExited, Signal: &signal}, wantErr: "the GitHub Actions runner installation failed (exited, exit code -1)"},
		{name: "failed", result: &ownsand.ExecResult{Kind: ownsand.ExecResultFailed}, wantErr: "the GitHub Actions runner installation failed (failed, exit code -1)"},
		{name: "sandbox ended", result: &ownsand.ExecResult{Kind: ownsand.ExecResultSandboxEnded}, wantErr: "the GitHub Actions runner installation failed (sandbox_ended, exit code -1)"},
		{name: "transport error", execErr: transportErr},
		{name: "context error", execErr: context.DeadlineExceeded},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := &work.Guest{
				Runner: &work.Runner{GuestOS: sandrunv1.GuestOs_GUEST_OS_LINUX},
				Client: prepareExecClient{exec: func(context.Context, ownsand.ExecParams) (*ownsand.ExecResult, error) {
					return tt.result, tt.execErr
				}},
			}
			err := a.Prepare(t.Context(), g)
			switch {
			case tt.execErr != nil:
				if err != tt.execErr {
					t.Fatalf("Prepare error = %v, want the original exec error %v", err, tt.execErr)
				}
			case tt.wantErr != "":
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("Prepare error = %v, want %q", err, tt.wantErr)
				}
			default:
				if err != nil {
					t.Fatalf("Prepare error = %v, want nil", err)
				}
			}
		})
	}
}

func TestPrepareDeadline(t *testing.T) {
	const timeout = time.Minute
	const contextTimeout = timeout + 30*time.Second
	a := &Adapter{l: &Layer{gh: &githubapi.Client{}, timing: Timing{PrepareTimeout: timeout}}}

	for _, name := range []string{"guest timeout", "earlier parent deadline", "parent cancellation"} {
		t.Run(name, func(t *testing.T) {
			parent := t.Context()
			switch name {
			case "earlier parent deadline":
				var cancel context.CancelFunc
				parent, cancel = context.WithTimeout(parent, 10*time.Second)
				defer cancel()
			case "parent cancellation":
				var cancel context.CancelFunc
				parent, cancel = context.WithCancel(parent)
				cancel()
			}
			parentDeadline, hasParentDeadline := parent.Deadline()
			var deadline time.Time
			var hasDeadline bool
			g := &work.Guest{
				Runner: &work.Runner{GuestOS: sandrunv1.GuestOs_GUEST_OS_LINUX},
				Client: prepareExecClient{exec: func(ctx context.Context, p ownsand.ExecParams) (*ownsand.ExecResult, error) {
					if p.Timeout != timeout {
						t.Errorf("guest timeout = %s, want %s", p.Timeout, timeout)
					}
					deadline, hasDeadline = ctx.Deadline()
					zero := int64(0)
					return &ownsand.ExecResult{Kind: ownsand.ExecResultExited, ExitCode: &zero}, ctx.Err()
				}},
			}
			before := time.Now()
			if err := a.Prepare(parent, g); err != parent.Err() {
				t.Fatalf("Prepare error = %v, want %v", err, parent.Err())
			}
			switch {
			case !hasDeadline:
				t.Fatal("exec context has no deadline")
			case hasParentDeadline:
				if !deadline.Equal(parentDeadline) {
					t.Errorf("exec deadline = %v, want parent deadline %v", deadline, parentDeadline)
				}
			default:
				if after := time.Now(); deadline.Before(before.Add(contextTimeout)) || deadline.After(after.Add(contextTimeout)) {
					t.Errorf("exec deadline = %v, want %s after Prepare starts", deadline, contextTimeout)
				}
			}
		})
	}
}
