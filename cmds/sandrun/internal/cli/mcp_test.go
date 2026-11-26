// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/delinoio/mono/cmds/internal/credstore"
	"github.com/delinoio/mono/go/sandrun/sandruntest"
)

// TestMCPCommand starts sandrun mcp on an in-memory transport: without a
// credential, every tool tells the user to run sandrun login or set
// SANDRUN_API_KEY, standard output stays free of anything but MCP messages,
// and logs go to standard error.
func TestMCPCommand(t *testing.T) {
	f := newFixture(t)
	f.env[EnvConfigDir] = t.TempDir() // no stored tokens
	ctx, cs := startMCPCommand(t, f)
	checkMCPCommandUnauthenticated(t, ctx, cs)
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
			LookupEnv: func(k string) (string, bool) { v, ok := f.env[k]; return v, ok },
			Getwd:     func() (string, error) { return f.wd, nil },
		})
	}()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return ctx, cs
}

func checkMCPCommandUnauthenticated(t *testing.T, ctx context.Context, cs *mcp.ClientSession) {
	t.Helper()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_sessions", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !res.IsError || !strings.Contains(text, "sandrun login") || !strings.Contains(text, EnvAPIKey) || !strings.HasPrefix(text, "unauthenticated") {
		t.Fatalf("tool without credential = %v %q", res.IsError, text)
	}
}
