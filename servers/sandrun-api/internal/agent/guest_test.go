// SPDX-License-Identifier: FSL-1.1-ALv2

package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/delinoio/mono/go/ownsand"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient"
	"github.com/delinoio/mono/servers/sandrun-api/internal/work"
)

type stepExecClient struct {
	ownsandclient.Client
	result *ownsand.ExecResult
	err    error
}

func (c stepExecClient) Exec(context.Context, string, ownsand.ExecParams) (*ownsand.ExecResult, error) {
	return c.result, c.err
}

type startExecClient struct {
	ownsandclient.Client
	calls int
}

func (c *startExecClient) Exec(_ context.Context, _ string, p ownsand.ExecParams) (*ownsand.ExecResult, error) {
	c.calls++
	return nil, p.OnEvent(ownsand.ExecEvent{Kind: ownsand.ExecEventStarted})
}

func TestStartProgramRejectsDoubleQuoteOnWindows(t *testing.T) {
	const doubleQuotedPool = `team"a`
	for _, tt := range []struct {
		name  string
		guest sandrunv1.GuestOs
		pool  string
	}{
		{name: "Windows rejects a double quote", guest: windows, pool: doubleQuotedPool},
		{name: "Windows accepts apostrophes", guest: windows, pool: "Bob's team’s pool"},
		{name: "Linux accepts a double quote", guest: linux, pool: doubleQuotedPool},
		{name: "macOS accepts a double quote", guest: sandrunv1.GuestOs_GUEST_OS_MACOS, pool: doubleQuotedPool},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &startExecClient{}
			guest := &work.Guest{Client: client, Runner: &work.Runner{GuestOS: tt.guest}}
			result, err := StartProgram(t.Context(), guest, Program{Args: []string{"agent", "worker", "--pool", tt.pool, "start"}})
			if tt.guest == windows && tt.pool == doubleQuotedPool {
				want := StepError{Step: "start the runner program", Code: -1, Msg: windowsDoubleQuoteMessage}
				var got *StepError
				if !errors.As(err, &got) || *got != want || client.calls != 0 || result.ProgramExecID != "" {
					t.Fatalf("StartProgram = (%+v, %#v), exec calls = %d; want empty result, %#v, and no exec", result, err, client.calls, want)
				}
				return
			}
			if err != nil || client.calls != 1 || result.ProgramExecID == "" {
				t.Fatalf("StartProgram = (%+v, %v), exec calls = %d; want an exec ID and one exec", result, err, client.calls)
			}
		})
	}
}

func TestRunStep(t *testing.T) {
	const step = "install runner"
	zero, nonzero := int64(0), int64(12)
	exitCodes := map[int64]string{nonzero: "installer failed"}
	transportErr := errors.New("connection lost")
	timeout := &StepError{Step: step, Code: -1, Msg: "timed out"}
	for _, tt := range []struct {
		name    string
		result  *ownsand.ExecResult
		execErr error
		want    *StepError
	}{
		{name: "timed out with zero exit", result: &ownsand.ExecResult{Kind: ownsand.ExecResultExited, ExitCode: &zero, TimedOut: true}, want: timeout},
		{name: "timed out with mapped exit", result: &ownsand.ExecResult{Kind: ownsand.ExecResultExited, ExitCode: &nonzero, TimedOut: true}, want: timeout},
		{name: "timed out without exit", result: &ownsand.ExecResult{Kind: ownsand.ExecResultFailed, TimedOut: true}, want: timeout},
		{name: "success", result: &ownsand.ExecResult{Kind: ownsand.ExecResultExited, ExitCode: &zero}},
		{name: "mapped exit", result: &ownsand.ExecResult{Kind: ownsand.ExecResultExited, ExitCode: &nonzero}, want: &StepError{Step: step, Code: nonzero, Msg: "installer failed"}},
		{name: "transport error", execErr: transportErr},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := &work.Guest{Client: stepExecClient{result: tt.result, err: tt.execErr}, Runner: &work.Runner{GuestOS: linux}}
			err := RunStep(t.Context(), g, step, ":", time.Second, exitCodes)
			if tt.execErr != nil {
				if !errors.Is(err, tt.execErr) || err.Error() != "install runner: connection lost" {
					t.Fatalf("RunStep error = %v, want wrapped transport error with step name", err)
				}
				return
			}
			if tt.want == nil {
				if err != nil {
					t.Fatalf("RunStep error = %v, want nil", err)
				}
				return
			}
			var got *StepError
			if !errors.As(err, &got) || *got != *tt.want {
				t.Fatalf("RunStep error = %#v, want %#v", err, tt.want)
			}
		})
	}
}
