// SPDX-License-Identifier: FSL-1.1-ALv2

package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// customBuildVersion is a running version that is not SemVer. Enrollment
// accepts such a build string, so the updater must still process a valid
// pinned target for it.
const customBuildVersion = "custom-build"

func TestUnparsableRunningVersionInstallsPinnedTarget(t *testing.T) {
	r := newRelease(t, nil)
	busy := &atomic.Bool{}
	busy.Store(true)
	u, inst, restarts := newUpdater(t, r, customBuildVersion, busy)
	if err := update(u, "1.1.0", false); err != nil {
		t.Fatal(err)
	}
	if s, target := u.State(); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_PENDING_IDLE || target != "1.1.0" {
		t.Fatalf("state %v %q", s, target)
	}
	if ok, _ := u.TryInstall(); ok || inst.installed.Load() != 0 {
		t.Fatal("installed while busy")
	}
	busy.Store(false)
	if ok, err := u.TryInstall(); !ok || err != nil || inst.installed.Load() != 1 || restarts.Load() != 1 {
		t.Fatalf("install: %v %v installs=%d restarts=%d", ok, err, inst.installed.Load(), restarts.Load())
	}
}

func TestUnparsableRunningVersionKeepsVerification(t *testing.T) {
	// A manifest that an untrusted key signed still fails with no install.
	r := newRelease(t, nil)
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	busy := &atomic.Bool{}
	u, inst, restarts := newUpdater(t, r, customBuildVersion, busy, otherPub)
	if err := update(u, "1.1.0", false); err == nil {
		t.Fatal("untrusted manifest signature accepted")
	}
	if s, _ := u.State(); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED {
		t.Fatalf("state %v", s)
	}
	if ok, _ := u.TryInstall(); ok || inst.installed.Load() != 0 || restarts.Load() != 0 {
		t.Fatal("installed after a failed verification")
	}
}

func TestUnparsableRunningVersionRejectsInvalidTarget(t *testing.T) {
	r := newRelease(t, nil)
	busy := &atomic.Bool{}
	// A target equal to the unparsable running string is still invalid.
	for _, target := range []string{"not-a-version", customBuildVersion} {
		u, inst, _ := newUpdater(t, r, customBuildVersion, busy)
		if err := update(u, target, false); err == nil {
			t.Fatalf("invalid target %q accepted", target)
		}
		if s, _ := u.State(); s != ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_FAILED {
			t.Fatalf("target %q: state %v", target, s)
		}
		if ok, _ := u.TryInstall(); ok || inst.installed.Load() != 0 {
			t.Fatalf("target %q: installed", target)
		}
	}
}

func TestLowerVersionLogNeedsSuccessfulComparison(t *testing.T) {
	const lowerLog = "agent update installs a lower version"
	for _, tc := range []struct {
		name, running string
		wantLog       bool
	}{
		{"lower semver running version", "1.2.0", true},
		{"unparsable running version", customBuildVersion, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRelease(t, nil)
			u, _, _ := newUpdater(t, r, tc.running, &atomic.Bool{})
			var logs strings.Builder
			u.opts.Logger = slog.New(slog.NewTextHandler(&logs, nil))
			target := "1.1.0"
			if err := update(u, target, false); err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(logs.String(), lowerLog); got != tc.wantLog {
				t.Fatalf("lower-version log = %v, want %v", got, tc.wantLog)
			}
		})
	}
}
