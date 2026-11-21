// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
)

// executeHandler drives handler.Execute with a fake runner. When stop is
// true, the test sends a stop request and the runner returns its outcome
// after the context ends; otherwise the runner returns at once.
func executeHandler(t *testing.T, code int, updated, stop bool) (h *handler, specific bool, exit uint32) {
	t.Helper()
	h = &handler{run: func(ctx context.Context, _ config.Config) (int, bool) {
		if stop {
			<-ctx.Done()
		}
		return code, updated
	}}
	requests := make(chan svc.ChangeRequest, 1)
	status := make(chan svc.Status, 8)
	if stop {
		requests <- svc.ChangeRequest{Cmd: svc.Stop}
	}
	type result struct {
		specific bool
		exit     uint32
	}
	res := make(chan result, 1)
	go func() {
		s, e := h.Execute(nil, requests, status)
		res <- result{s, e}
	}()
	select {
	case r := <-res:
		return h, r.specific, r.exit
	case <-time.After(10 * time.Second):
		t.Fatal("Execute did not return")
	}
	return nil, false, 0
}

func TestHandlerReportsTheRestartStatus(t *testing.T) {
	for _, tc := range []struct {
		name          string
		code          int
		updated, stop bool
		specific      bool
		exit          uint32
	}{
		{"installed update", 0, true, false, true, 75},
		{"error", 1, false, false, true, 1},
		{"not enrolled or removed", 3, false, false, false, 0},
		{"clean stop", 0, false, false, false, 0},
		{"stop request while the agent later fails", 1, false, true, false, 0},
	} {
		h, specific, exit := executeHandler(t, tc.code, tc.updated, tc.stop)
		if specific != tc.specific || exit != tc.exit {
			t.Errorf("%s: Execute = (%v, %d), want (%v, %d)", tc.name, specific, exit, tc.specific, tc.exit)
		}
		// The public exit code of `run` does not change with the reported status.
		if h.code != tc.code {
			t.Errorf("%s: public exit code = %d, want %d", tc.name, h.code, tc.code)
		}
	}
}
