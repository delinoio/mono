// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMCPCommand starts tide mcp on an in-memory transport: without a
// credential, every tool tells the user to run tide login or set
// TIDE_API_KEY, standard output stays free of anything but MCP messages,
// and logs go to standard error.
func TestMCPCommand(t *testing.T) {
	f := newFixture(t)
	f.env[EnvConfigDir] = t.TempDir() // no stored tokens
	serverT, clientT := mcp.NewInMemoryTransports()
	old := mcpTransport
	mcpTransport = func(Env) mcp.Transport { return serverT }
	t.Cleanup(func() { mcpTransport = old })

	var out, errb bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
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
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_sessions", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !res.IsError || !strings.Contains(text, "tide login") || !strings.Contains(text, EnvAPIKey) || !strings.HasPrefix(text, "unauthenticated") {
		t.Fatalf("tool without credential = %v %q", res.IsError, text)
	}
	cs.Close()
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("tide mcp exited %d: %s", code, errb.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tide mcp did not stop")
	}
	if out.Len() != 0 {
		t.Fatalf("tide mcp wrote to standard output: %q", out.String())
	}
	if !strings.Contains(errb.String(), "tide mcp serves") {
		t.Fatalf("logs = %q", errb.String())
	}
}
