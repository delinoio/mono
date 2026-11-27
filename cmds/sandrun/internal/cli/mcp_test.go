// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/delinoio/mono/cmds/internal/credstore"
	"github.com/delinoio/mono/go/sandrun/sandruntest"
)

const unknownTransferID = "00000000-0000-4000-8000-000000000002"

// TestMCPCommand checks sign-in guidance and wait validation through an
// in-memory transport. The server writes logs to standard error.
func TestMCPCommand(t *testing.T) {
	f := newFixture(t)
	f.env[EnvConfigDir] = t.TempDir() // no stored tokens
	ctx, cs := startMCPCommand(t, f)
	got := mcpCommandToolError(t, ctx, cs, "transfer_status", map[string]any{
		"transfer_id": unknownTransferID, "wait_seconds": 0,
	})
	if want := checkMCPCommandUnauthenticated(t, ctx, cs); got != want {
		t.Errorf("transfer_status without credential = %q, want %q", got, want)
	}
	got = mcpCommandToolError(t, ctx, cs, "transfer_status", map[string]any{
		"transfer_id": unknownTransferID, "wait_seconds": 51,
	})
	if !strings.HasPrefix(got, "invalid_argument:") || !strings.Contains(got, "wait_seconds") {
		t.Errorf("transfer_status with invalid wait = %q", got)
	}
}

func TestMCPCommandUnknownTransferWithAPIKey(t *testing.T) {
	f := newFixture(t)
	f.env[EnvConfigDir] = t.TempDir() // no stored tokens
	f.env[EnvAPIKey] = "synthetic-key"
	f.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected HTTP request: %s %s", r.Method, r.URL.Path)
		return nil, errors.New("unexpected HTTP request")
	})}
	ctx, cs := startMCPCommand(t, f)
	got := mcpCommandToolError(t, ctx, cs, "transfer_status", map[string]any{
		"transfer_id": unknownTransferID, "wait_seconds": 0,
	})
	want := "not_found: no transfer with this transfer_id is available in this server process. The process keeps only the 64 most recent sync or download outcomes, including failures"
	if got != want {
		t.Errorf("transfer_status with API key = %q, want %q", got, want)
	}
}

func TestMCPCommandPicksUpLoginAfterStart(t *testing.T) {
	f := newFixture(t)
	creds, err := (credstore.Store{Dir: f.cfg, Program: "sandrun"}).Load()
	if err != nil {
		t.Fatal(err)
	}
	f.env[EnvConfigDir] = t.TempDir()
	ctx, cs := startMCPCommand(t, f)
	checkMCPCommandUnauthenticated(t, ctx, cs)

	st := credstore.Store{Dir: f.env[EnvConfigDir], Program: "sandrun"}
	if err := st.Save(creds); err != nil {
		t.Fatal(err)
	}
	var calls sync.WaitGroup
	for range 4 {
		calls.Go(func() {
			res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_sessions", Arguments: map[string]any{}})
			if err != nil {
				t.Errorf("call list_sessions after login: %v", err)
			} else if res.IsError {
				t.Errorf("list_sessions returned an error after login: %v", res.Content[0].(*mcp.TextContent).Text)
			}
		})
	}
	calls.Wait()
	if f.srv.LastCredential() != sandruntest.CredentialAccessToken {
		t.Error("the stored Logto access token was not used")
	}
}

func startMCPCommand(t *testing.T, f *fixture) (context.Context, *mcp.ClientSession) {
	t.Helper()
	serverT, clientT := mcp.NewInMemoryTransports()
	old := mcpTransport
	mcpTransport = func(Env) mcp.Transport { return serverT }
	t.Cleanup(func() { mcpTransport = old })

	var out, errb bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	done := make(chan int, 1)
	t.Cleanup(func() {
		cancel()
		select {
		case code := <-done:
			if code != 0 {
				t.Fatalf("sandrun mcp exited %d: %s", code, errb.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatal("sandrun mcp did not stop")
		}
		if out.Len() != 0 {
			t.Fatalf("sandrun mcp wrote to standard output: %q", out.String())
		}
		if !strings.Contains(errb.String(), "sandrun mcp serves") {
			t.Fatalf("logs = %q", errb.String())
		}
	})
	go func() {
		done <- Run(ctx, Env{
			Args: []string{"mcp", "--org", f.org, "--pass-env", "NPM_TOKEN"}, Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb,
			LookupEnv:  func(k string) (string, bool) { v, ok := f.env[k]; return v, ok },
			HTTPClient: f.httpClient,
			Getwd:      func() (string, error) { return f.wd, nil },
		})
	}()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return ctx, cs
}

func checkMCPCommandUnauthenticated(t *testing.T, ctx context.Context, cs *mcp.ClientSession) string {
	t.Helper()
	text := mcpCommandToolError(t, ctx, cs, "list_sessions", map[string]any{})
	if !strings.HasPrefix(text, "unauthenticated:") || !strings.Contains(text, "sandrun login") || !strings.Contains(text, EnvAPIKey) {
		t.Fatalf("list_sessions without credential = %q", text)
	}
	return text
}

func mcpCommandToolError(t *testing.T, ctx context.Context, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if !res.IsError || len(res.Content) != 1 {
		t.Fatalf("%s result = %+v, want one tool error", name, res)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("%s error content has type %T, want *mcp.TextContent", name, res.Content[0])
	}
	return text.Text
}
