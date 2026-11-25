// SPDX-License-Identifier: FSL-1.1-ALv2

package githubapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

func TestDelayedUnauthorizedPreservesReplacementInstallationToken(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var mints, requests atomic.Int32
	token := func(number int32) string { return fmt.Sprintf("test-installation-token-%d", number) }
	delayedStarted := make(chan struct{})
	releaseDelayed := make(chan struct{})
	steps := []struct {
		tokenNumber int32
		status      int
	}{
		{1, http.StatusUnauthorized}, // Delayed until after token 2 succeeds.
		{1, http.StatusUnauthorized},
		{2, http.StatusOK},
		{2, http.StatusOK},
		{2, http.StatusUnauthorized},
		{3, http.StatusOK},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/1/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(map[string]string{"token": token(mints.Add(1))}); err != nil {
			t.Errorf("encode token response: %v", err)
		}
	})
	mux.HandleFunc("GET /orgs/acme/actions/runner-groups", func(w http.ResponseWriter, r *http.Request) {
		call := int(requests.Add(1))
		if call > len(steps) {
			t.Errorf("unexpected runner group request %d", call)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		step := steps[call-1]
		if r.Header.Get("Authorization") != "Bearer "+token(step.tokenNumber) {
			t.Errorf("runner group request %d used the wrong bearer", call)
		}
		if call == 1 {
			close(delayedStarted)
			select {
			case <-releaseDelayed:
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(step.status)
		if step.status == http.StatusOK {
			_, _ = fmt.Fprint(w, `{"total_count":0,"runner_groups":[]}`)
		}
	})
	upstream := httptest.NewServer(mux)
	t.Cleanup(upstream.Close)
	f := githubfake.New(nil)
	t.Cleanup(f.Server.Close)
	cfg := f.Config()
	cfg.APIOrigin = upstream.URL
	c := githubapi.New(cfg, upstream.Client(), slog.New(slog.DiscardHandler), nil)

	list := func() error {
		_, err := c.RunnerGroups(ctx, 1, "acme")
		return err
	}
	requireUnauthorized := func(label string, err error) {
		t.Helper()
		var apiErr *githubapi.Error
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized || apiErr.Op != "list_runner_groups" {
			t.Fatalf("%s: got %v, want the runner group HTTP 401", label, err)
		}
	}
	requireSuccess := func(label string, wantMints int32) {
		t.Helper()
		if err := list(); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if got := mints.Load(); got != wantMints {
			t.Fatalf("%s: got %d token mints, want %d", label, got, wantMints)
		}
	}

	delayedResult := make(chan error, 1)
	go func() { delayedResult <- list() }()
	select {
	case <-delayedStarted:
	case err := <-delayedResult:
		t.Fatalf("request returned before reaching the delayed handler: %v", err)
	case <-ctx.Done():
		t.Fatal("timed out waiting for the delayed request")
	}
	requireUnauthorized("reject the initial token", list())
	requireSuccess("mint and use the replacement token", 2)
	close(releaseDelayed)
	requireUnauthorized("return the delayed 401", <-delayedResult)
	requireSuccess("reuse the replacement after the delayed 401", 2)
	requireUnauthorized("reject the replacement token", list())
	requireSuccess("refresh after the current token is rejected", 3)
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

func TestFailedDeliveriesBeyondPage100(t *testing.T) {
	since := time.Date(2026, time.November, 24, 0, 0, 0, 0, time.UTC)
	now := since
	f := githubfake.New(func() time.Time { return now })
	t.Cleanup(f.Server.Close)
	c := githubapi.New(f.Config(), nil, slog.New(slog.DiscardHandler), func() time.Time { return now })

	atCutoff := f.SendLost("workflow_job", nil)
	f.Update(func() { now = since.Add(time.Hour) })
	for range 10_000 {
		f.SendLost("workflow_job", nil)
	}

	list, err := c.FailedDeliveries(t.Context(), since)
	if err != nil || len(list) != 10_001 || f.Calls("list_hook_deliveries") != 101 {
		t.Fatalf("list = %d deliveries, %v, %d page requests", len(list), err, f.Calls("list_hook_deliveries"))
	}
	if last := list[len(list)-1]; last.GUID != atCutoff || !last.DeliveredAt.Equal(since) {
		t.Fatalf("last delivery = %+v, want %s at %s", last, atCutoff, since)
	}
}

func TestFailedDeliveriesPagination(t *testing.T) {
	since := time.Date(2026, time.November, 24, 0, 0, 0, 0, time.UTC)
	f := githubfake.New(nil)
	t.Cleanup(f.Server.Close)
	var foreignCalls atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignCalls.Add(1)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	t.Cleanup(foreign.Close)

	for _, tt := range []struct {
		name      string
		wantPages int32
		wantID    int64
	}{
		{name: "end", wantPages: 1, wantID: 1},
		{name: "cutoff", wantPages: 1, wantID: 1},
		{name: "foreign origin", wantPages: 1, wantID: 1},
		{name: "empty page with cursor", wantPages: 2, wantID: 2},
		{name: "cycle", wantPages: 2},
		{name: "http error", wantPages: 2},
		{name: "canceled", wantPages: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				page := calls.Add(1)
				if page > tt.wantPages {
					t.Error("requested an extra page")
					http.Error(w, "unexpected request", http.StatusInternalServerError)
					return
				}
				items := []githubapi.Delivery{{ID: int64(page), DeliveredAt: since, StatusCode: 500}}
				origin := "http://" + r.Host
				var next string
				if page == 1 {
					switch tt.name {
					case "cutoff":
						items = append(items, githubapi.Delivery{ID: 2, DeliveredAt: since.Add(-time.Second), StatusCode: 500})
						next = "cursor=2"
					case "foreign origin":
						origin = foreign.URL
						next = "cursor=2"
					case "empty page with cursor":
						items = []githubapi.Delivery{}
						next = "cursor=2"
					case "cycle", "http error", "canceled":
						next = "cursor=2"
					}
				} else {
					switch tt.name {
					case "cycle":
						next = "per_page=100&status=failure"
					case "http error":
						http.Error(w, "listing failed", http.StatusBadGateway)
						return
					case "canceled":
						cancel()
						<-r.Context().Done()
						return
					}
				}
				if next != "" {
					w.Header().Set("Link", fmt.Sprintf(`<%s/app/hook/deliveries?%s>; rel="next"`, origin, next))
				}
				if err := json.NewEncoder(w).Encode(items); err != nil {
					t.Errorf("encode deliveries: %v", err)
				}
			}))
			t.Cleanup(upstream.Close)
			cfg := f.Config()
			cfg.APIOrigin = upstream.URL
			c := githubapi.New(cfg, upstream.Client(), slog.New(slog.DiscardHandler), nil)
			list, err := c.FailedDeliveries(ctx, since)

			switch tt.name {
			case "cycle":
				if err == nil || !strings.Contains(err.Error(), "pagination cycle") {
					t.Errorf("cycle error = %v", err)
				}
			case "http error":
				if githubapi.StatusOf(err) != http.StatusBadGateway {
					t.Errorf("later page error = %v", err)
				}
			case "canceled":
				if !errors.Is(err, context.Canceled) {
					t.Errorf("cancellation error = %v", err)
				}
			default:
				if err != nil {
					t.Errorf("list error = %v", err)
				}
			}
			if tt.wantID == 0 && len(list) != 0 || tt.wantID != 0 && (len(list) != 1 || list[0].ID != tt.wantID) {
				t.Errorf("got %v, want only delivery %d (0 means none)", list, tt.wantID)
			}
			if pages := calls.Load(); pages != tt.wantPages {
				t.Errorf("listed %d pages, want %d", pages, tt.wantPages)
			}
		})
	}
	if calls := foreignCalls.Load(); calls != 0 {
		t.Errorf("foreign origin received %d requests", calls)
	}
}
