// SPDX-License-Identifier: FSL-1.1-ALv2

package cursor

import (
	"strings"
	"testing"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/agent"
)

func TestStartScriptPoolName(t *testing.T) {
	claim := &agent.Claim{WorkerID: "tide-1", Scope: "Bob’s pool"}
	for _, tt := range []struct {
		name  string
		guest tidev1.GuestOs
		dir   string
		want  string
	}{
		{"Windows", tidev1.GuestOs_GUEST_OS_WINDOWS, `C:\ProgramData\tide\runners\r1`, `'--pool' 'Bob’’s pool' '--worker-dir'`},
		{"Linux", tidev1.GuestOs_GUEST_OS_LINUX, "/tmp/tide-runner-r1", `'--pool' 'Bob’s pool' '--worker-dir'`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			script := agent.StartScript(tt.guest, Program(tt.dir, claim, 60))
			if !strings.Contains(script, tt.want) {
				t.Fatalf("script lacks %q:\n%s", tt.want, script)
			}
		})
	}
}
