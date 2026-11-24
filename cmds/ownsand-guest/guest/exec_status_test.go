// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"fmt"
	"testing"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func requireExplicitSuccess(ev *ownsandv1.ExecEvent) error {
	return requireExplicitZeroExit(ev, false)
}

// Check the oneof variants because the scalar getters also return zero for
// missing fields and other variants.
func requireExplicitZeroExit(ev *ownsandv1.ExecEvent, wantTimedOut bool) error {
	exit, ok := ev.GetEvent().(*ownsandv1.ExecEvent_Exited)
	if !ok || exit == nil || exit.Exited == nil {
		return fmt.Errorf("terminal event is not an exit: %v", ev)
	}
	code, ok := exit.Exited.GetStatus().(*ownsandv1.ExecExited_ExitCode)
	if !ok || code == nil {
		return fmt.Errorf("exit has no explicit exit code: %v", ev)
	}
	if code.ExitCode != 0 || exit.Exited.GetTimedOut() != wantTimedOut {
		return fmt.Errorf("want exit code 0 and timed_out=%t, got: %v", wantTimedOut, ev)
	}
	return nil
}

func TestRequireExplicitSuccess(t *testing.T) {
	exited := func(x *ownsandv1.ExecExited) *ownsandv1.ExecEvent {
		return &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Exited{Exited: x}}
	}
	for _, timedOut := range []bool{false, true} {
		cases := []struct {
			name string
			ev   *ownsandv1.ExecEvent
			zero bool
		}{
			{"nil event", nil, false},
			{"failed event", &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Failed{
				Failed: &ownsandv1.ExecFailed{Message: "boom"}}}, false},
			{"nil exit variant", &ownsandv1.ExecEvent{Event: (*ownsandv1.ExecEvent_Exited)(nil)}, false},
			{"nil exit payload", exited(nil), false},
			{"unset status", exited(&ownsandv1.ExecExited{TimedOut: timedOut}), false},
			{"nil exit code", exited(&ownsandv1.ExecExited{
				Status: (*ownsandv1.ExecExited_ExitCode)(nil), TimedOut: timedOut}), false},
			{"signal 15", exited(&ownsandv1.ExecExited{
				Status: &ownsandv1.ExecExited_Signal{Signal: 15}, TimedOut: timedOut}), false},
			{"exit 7", exited(&ownsandv1.ExecExited{
				Status: &ownsandv1.ExecExited_ExitCode{ExitCode: 7}, TimedOut: timedOut}), false},
			{"explicit zero", exited(&ownsandv1.ExecExited{
				Status: &ownsandv1.ExecExited_ExitCode{ExitCode: 0}, TimedOut: timedOut}), true},
		}
		for _, tc := range cases {
			t.Run(fmt.Sprintf("timed_out=%t/%s", timedOut, tc.name), func(t *testing.T) {
				if err := requireExplicitSuccess(tc.ev); (err == nil) != (tc.zero && !timedOut) {
					t.Errorf("expected timed_out=false, got error %v for %v", err, tc.ev)
				}
				if err := requireExplicitZeroExit(tc.ev, true); (err == nil) != (tc.zero && timedOut) {
					t.Errorf("expected timed_out=true, got error %v for %v", err, tc.ev)
				}
			})
		}
	}
}
