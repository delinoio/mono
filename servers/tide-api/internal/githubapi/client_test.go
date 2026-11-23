// SPDX-License-Identifier: FSL-1.1-ALv2

package githubapi_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/tide-api/internal/githubapi"
	"github.com/delinoio/mono/servers/tide-api/internal/githubapi/githubfake"
)

func newClient(t *testing.T) (*githubapi.Client, *githubfake.Fake, *bytes.Buffer) {
	t.Helper()
	f := githubfake.New(nil)
	t.Cleanup(f.Server.Close)
	logs := &bytes.Buffer{}
	c := githubapi.New(f.Config(), nil, slog.New(slog.NewJSONHandler(logs, nil)), nil)
	return c, f, logs
}

// TestSignature checks the documented test vector and the rejections.
func TestSignature(t *testing.T) {
	secret, body := []byte("It's a Secret to Everybody"), []byte("Hello, World!")
	want := "sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"
	if got := githubapi.Sign(secret, body); got != want {
		t.Fatalf("sign = %s", got)
	}
	if !githubapi.VerifySignature(secret, body, want) || !githubapi.VerifySignature(secret, body, "sha256="+strings.ToUpper(want[7:])) {
		t.Fatal("valid signature rejected")
	}
	for _, bad := range []string{"", "sha256=", want[:len(want)-1], "sha1=" + want[7:], want[:len(want)-1] + "0", "sha256=" + strings.Repeat("zz", 32)} {
		if githubapi.VerifySignature(secret, body, bad) {
			t.Errorf("accepted %q", bad)
		}
	}
	if githubapi.VerifySignature([]byte("other"), body, want) || githubapi.VerifySignature(secret, []byte("Hello, World?"), want) {
		t.Fatal("accepted another secret or body")
	}
}

func TestInstallationTokenCache(t *testing.T) {
	c, f, _ := newClient(t)
	f.AddInstallation(1, "acme", "Organization")
	ctx := context.Background()
	t1, err := c.InstallationToken(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	t2, _ := c.InstallationToken(ctx, 1)
	if t1 != t2 || f.Calls("create_installation_token") != 1 {
		t.Fatalf("token not cached: %d calls", f.Calls("create_installation_token"))
	}
	if _, err := c.InstallationToken(ctx, 2); !githubapi.IsNotFound(err) {
		t.Fatalf("unknown installation: %v", err)
	}
	f.SetSuspended(1, true)
	if _, err := c.RunnerGroups(ctx, 1, "acme"); githubapi.StatusOf(err) != 403 {
		t.Fatalf("suspended: %v", err)
	}
}

func TestRunners(t *testing.T) {
	c, f, logs := newClient(t)
	f.AddInstallation(1, "acme", "Organization")
	ctx := context.Background()
	groups, err := c.RunnerGroups(ctx, 1, "acme")
	if err != nil || len(groups) != 2 || !groups[0].Default {
		t.Fatalf("groups = %v, %v", groups, err)
	}
	jit, err := c.GenerateJITConfig(ctx, 1, "acme", "tide-r1", 1, []string{"tide-linux"})
	if err != nil || jit.Runner.ID == 0 || jit.EncodedJITConfig == "" {
		t.Fatalf("jit = %v, %v", jit, err)
	}
	if _, err := c.GenerateJITConfig(ctx, 1, "acme", "tide-r1", 1, []string{"tide-linux"}); !errors.Is(err, githubapi.ErrRunnerNameTaken) {
		t.Fatalf("duplicate name: %v", err)
	}
	found, err := c.FindRunner(ctx, 1, "acme", "tide-r1")
	if err != nil || found == nil || found.ID != jit.Runner.ID {
		t.Fatalf("find = %v, %v", found, err)
	}
	if found, _ := c.FindRunner(ctx, 1, "acme", "tide-r2"); found != nil {
		t.Fatal("found a missing runner")
	}
	f.SetBusy("tide-r1", true)
	if err := c.DeleteRunner(ctx, 1, "acme", jit.Runner.ID); !errors.Is(err, githubapi.ErrRunnerRefused) {
		t.Fatalf("busy delete: %v", err)
	}
	f.SetBusy("tide-r1", false)
	if err := c.DeleteRunner(ctx, 1, "acme", jit.Runner.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteRunner(ctx, 1, "acme", jit.Runner.ID); err != nil {
		t.Fatalf("delete of a removed runner: %v", err)
	}
	if strings.Contains(logs.String(), jit.EncodedJITConfig) || strings.Contains(logs.String(), "ghs_") {
		t.Fatal("logs contain a secret")
	}
	if !strings.Contains(logs.String(), `"status":409`) || !strings.Contains(logs.String(), `"status":422`) {
		t.Fatalf("errors not logged with their status: %s", logs.String())
	}
}

func TestOAuth(t *testing.T) {
	c, f, logs := newClient(t)
	f.AddInstallation(1, "acme", "Organization", "octo")
	f.AddInstallation(2, "octo", "User", "octo")
	f.AddInstallation(3, "other", "Organization", "someone")
	f.AddCode("code-1", "octo")
	ctx := context.Background()
	if _, err := c.ExchangeCode(ctx, "wrong"); !errors.Is(err, githubapi.ErrBadCode) {
		t.Fatalf("bad code: %v", err)
	}
	token, err := c.ExchangeCode(ctx, "code-1")
	if err != nil || !strings.HasPrefix(token, "ghu_") {
		t.Fatalf("token: %v", err)
	}
	list, err := c.UserInstallations(ctx, token)
	if err != nil || len(list) != 2 {
		t.Fatalf("installations = %v, %v", list, err)
	}
	for _, inst := range list {
		if inst.IsOrganization() != (inst.ID == 1) {
			t.Errorf("installation %d organization = %v", inst.ID, inst.IsOrganization())
		}
	}
	if strings.Contains(logs.String(), "code-1") || strings.Contains(logs.String(), token) {
		t.Fatal("logs contain a secret")
	}
	if got := c.InstallURL("s t"); got != f.Server.URL+"/apps/tide-test/installations/new?state=s+t" {
		t.Fatalf("install url = %s", got)
	}
}

func TestFailedDeliveries(t *testing.T) {
	now := time.Now()
	c, f, _ := newClient(t)
	f.TargetClient = f.Server.Client()
	f.Target = f.Server.URL + "/nowhere" // answers 404
	for i := 0; i < 250; i++ {
		f.Send("workflow_job", map[string]int{"i": i})
	}
	list, err := c.FailedDeliveries(context.Background(), now.Add(-time.Hour))
	if err != nil || len(list) != 250 || f.Calls("list_hook_deliveries") != 3 {
		t.Fatalf("list = %d, %v, calls %d", len(list), err, f.Calls("list_hook_deliveries"))
	}
	if list[0].ID < list[1].ID {
		t.Fatal("not newest first")
	}
	if err := c.Redeliver(context.Background(), list[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := c.Redeliver(context.Background(), 1); githubapi.StatusOf(err) != 404 {
		t.Fatalf("unknown delivery: %v", err)
	}
}
