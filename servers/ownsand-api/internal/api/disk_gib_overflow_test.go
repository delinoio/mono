// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"math"
	"testing"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/ownsand/validate"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// TestCreateSandboxRejectsDiskGibOverflow sends a disk_gib whose byte count
// does not fit in a signed 64-bit integer and expects invalid_argument that
// names resources.disk_gib, before scheduling, with no sandbox record. The
// largest representable size passes the validation and, with a device that
// has less disk, fails the capacity check instead.
func TestCreateSandboxRejectsDiskGibOverflow(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	setupAgent(t, env, org, nil)
	for name, disk := range map[string]int64{
		"one over max": validate.MaxDiskGiB + 1,
		"max int64":    math.MaxInt64,
	} {
		req := createReq(org.ID)
		req.Resources.DiskGib = disk
		_, err := org.OwnerClient.Sandbox.CreateSandbox(ctx, connect.NewRequest(req))
		requireInvalidNaming(t, env, "disk_gib "+name, err, "resources.disk_gib")
	}
	if n := countRows(t, env, "SELECT count(*) FROM sandboxes WHERE organization_id = $1", org.ID); n != 0 {
		t.Fatalf("sandboxes after the rejected creates = %d, want 0", n)
	}
	req := createReq(org.ID)
	req.Resources.DiskGib = validate.MaxDiskGiB
	_, err := org.OwnerClient.Sandbox.CreateSandbox(ctx, connect.NewRequest(req))
	if testenv.Code(err) == connect.CodeInvalidArgument {
		t.Fatalf("largest representable disk_gib was rejected as invalid: %v", err)
	}
}
