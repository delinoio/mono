// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
)

func TestImageBuildRefusedBeforeOpeningStore(t *testing.T) {
	for _, held := range []bool{false, true} {
		name, want := "pending", store.ErrRemovalPending
		if held {
			name, want = "exclusive", store.ErrEnrollmentBusy
		}
		t.Run(name, func(t *testing.T) {
			clearAgentEnv(t)
			st, err := store.OpenForRead(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if held {
				unlock, err := st.LockEnrollment(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
			} else if _, err := st.UpdateAgent(func(s *store.AgentState) error {
				s.Removal.NoticeReceived = true
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			var stdout, stderr bytes.Buffer
			code := run([]string{"image", "build", "--os", "macos", "--state-dir", st.Dir(), "--no-color"}, nil, &stdout, &stderr)
			if code != exitError || !strings.Contains(stderr.String(), want.Error()) {
				t.Fatalf("image build = %d, %q, want %d and %v", code, stderr.String(), exitError, want)
			}
			for _, sub := range []string{store.ImagesDir, store.SandboxesDir, store.SnapshotsDir, store.VMsDir, store.UpdateDir} {
				if _, err := os.Stat(st.Path(sub)); !os.IsNotExist(err) {
					t.Errorf("refused build changed %s: %v", sub, err)
				}
			}
		})
	}
}
