// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/ownsand/validate"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// TestCreateSandboxRejectsDiskGibOverflow sends a disk_gib whose byte count
// does not fit in a signed 64-bit integer and expects invalid_argument that
// names resources.disk_gib, before scheduling, with no sandbox record. The
// largest representable size reaches the scheduler, which rejects it for
// exceeding the device's disk limit and leaves no sandbox record.
func TestCreateSandboxRejectsDiskGibOverflow(t *testing.T) {
	t.Parallel()
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
	const sandboxCountQuery = "SELECT count(*) FROM sandboxes WHERE organization_id = $1"
	if n := countRows(t, env, sandboxCountQuery, org.ID); n != 0 {
		t.Fatalf("sandboxes after the rejected creates = %d, want 0", n)
	}
	req := createReq(org.ID)
	req.Resources.DiskGib = validate.MaxDiskGiB
	_, err := org.OwnerClient.Sandbox.CreateSandbox(ctx, connect.NewRequest(req))
	const wantMessage = "no device can run the sandbox: the request needs more disk than the device limit"
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeFailedPrecondition || connectErr.Message() != wantMessage {
		t.Fatalf("create with maximum disk_gib = %v, want %v: %q", err, connect.CodeFailedPrecondition, wantMessage)
	}
	if n := countRows(t, env, sandboxCountQuery, org.ID); n != 0 {
		t.Fatalf("sandboxes after the maximum disk_gib request = %d, want 0", n)
	}
}
