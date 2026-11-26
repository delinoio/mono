// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"io"
	"log/slog"
	"regexp"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/delinoio/mono/cmds/sandrun/internal/mcpserver"
	"github.com/delinoio/mono/go/sandrun"
)

// envName is the form of a --pass-env name.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// mcpTransport returns the transport of sandrun mcp; tests replace it.
var mcpTransport = func(Env) mcp.Transport { return &mcp.StdioTransport{} }

// mcp serves the MCP server on standard input and output. Standard output
// carries only MCP messages; logs go to standard error.
func (a *app) mcp(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	var passEnv multiFlag
	fs.Var(&passEnv, "pass-env", "name of a local environment variable that every exec receives (repeatable)")
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	for _, n := range passEnv {
		if !envName.MatchString(n) {
			return usageErr("--pass-env %q is not an environment variable name", n)
		}
	}
	if a.fmt == FormatJSON {
		return usageErr("sandrun mcp does not take --output")
	}
	org, err := a.requireOrg()
	if err != nil {
		return err
	}
	wd, err := a.env.Getwd()
	if err != nil {
		return err
	}
	level := slog.LevelInfo
	if a.g.verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(newLogHandler(a.env.Stderr, a.color, level))
	// The SDK client logs through the same handler.
	a.logger = logger
	// Serialize SDK initialization across tool calls. If it fails, the next
	// call tries again and can load credentials saved by sandrun login.
	clientLock := make(chan struct{}, 1)
	srv := mcpserver.New(mcpserver.Config{
		Client: func(ctx context.Context) (*sandrun.Client, error) {
			select {
			case clientLock <- struct{}{}:
				defer func() { <-clientLock }()
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			client, err := a.sdk()
			return client, describeCredentialError(err)
		},
		OrganizationID: org,
		PassEnv:        passEnv,
		LookupEnv:      a.env.LookupEnv,
		WorkDir:        wd,
		Version:        Version,
		Logger:         logger,
	})
	logger.Info("sandrun mcp serves on standard input and output", "organization_id", org, "pass_env_count", len(passEnv))
	err = srv.Run(ctx, mcpTransport(a.env))
	if ctx.Err() != nil || err == nil || err == io.EOF {
		return nil
	}
	return err
}

// describeCredentialError keeps a credential error readable for the agent.
func describeCredentialError(err error) error {
	var ee *exitError
	if asExit(err, &ee) {
		return errString(ee.msg)
	}
	return err
}

type errString string

func (e errString) Error() string { return string(e) }

func asExit(err error, target **exitError) bool {
	e, ok := err.(*exitError)
	if ok {
		*target = e
	}
	return ok
}
