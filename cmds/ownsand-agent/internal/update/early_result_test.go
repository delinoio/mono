// SPDX-License-Identifier: FSL-1.1-ALv2

package update

import (
	"errors"
	"os"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/internal/semver"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestEarlyUpdateResults(t *testing.T) {
	const running = "1.0.0"
	for _, tc := range []struct {
		name, target, wantTarget, wantError string
		wantState                           ownsandv1.AgentUpdateState
	}{
		{"same version", running, "", "", ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE},
		{"invalid target", "invalid", "invalid", `semver: "invalid" is not MAJOR.MINOR.PATCH`, ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED},
		{"missing manifest", "1.1.0", "1.1.0", "update: no release manifest URL is configured", ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, scenario := range []struct {
				name string
				err  error
			}{
				{"current", nil},
				{"superseded", ErrSuperseded},
				{"removal", ErrRemovalAborted},
			} {
				t.Run(scenario.name, func(t *testing.T) {
					st, err := store.Open(t.TempDir())
					if err != nil {
						t.Fatal(err)
					}
					u, err := New(Options{Store: st, Running: running})
					if err != nil {
						t.Fatal(err)
					}
					cmd := &ownsandv1.UpdateAgentCommand{TargetVersion: tc.target}
					wantState, wantTarget := tc.wantState, tc.wantTarget
					var wantArtifact string
					if scenario.err == nil {
						var errText string
						if err := u.Handle(t.Context(), cmd); err != nil {
							errText = err.Error()
						}
						if errText != tc.wantError {
							t.Fatalf("command error = %q, want %q", errText, tc.wantError)
						}
					} else {
						// Suspend the command after admission. Invalidation runs
						// before the preparation phase can record the early result.
						order := u.NextOrder()
						u.applied = order
						if scenario.err == ErrSuperseded {
							u.applied = u.NextOrder()
							if err := u.queueVerified(t.Context(), u.applied, "1.2.0", []byte("newer artifact")); err != nil {
								t.Fatal(err)
							}
							wantArtifact = u.artifact
						} else {
							u.FenceForRemoval()
						}
						wantState, wantTarget = u.State()
						comparison, compareErr := semver.CompareStrings(tc.target, running)
						if err := u.prepareAdmittedUpdate(t.Context(), cmd, order, comparison, compareErr); !errors.Is(err, scenario.err) {
							t.Fatalf("command error = %v, want %v", err, scenario.err)
						}
					}
					if state, target := u.State(); state != wantState || target != wantTarget {
						t.Fatalf("state = %s, target = %q, want %s and %q", state, target, wantState, wantTarget)
					}
					if u.artifact != wantArtifact {
						t.Fatalf("artifact = %q, want %q", u.artifact, wantArtifact)
					}
					entries, err := os.ReadDir(st.Path(store.UpdateDir))
					if err != nil {
						t.Fatal(err)
					}
					if scenario.err == ErrSuperseded {
						data, err := os.ReadFile(wantArtifact)
						if len(entries) != 1 || err != nil || string(data) != "newer artifact" {
							t.Fatalf("newer artifact changed: entries=%v data=%q err=%v", entries, data, err)
						}
					} else if len(entries) != 0 {
						t.Fatalf("unexpected update artifacts: %v", entries)
					}
				})
			}
		})
	}
}
