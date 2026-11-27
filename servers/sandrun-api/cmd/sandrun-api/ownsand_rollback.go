// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/delinoio/mono/servers/sandrun-api/internal/config"
	"github.com/delinoio/mono/servers/sandrun-api/internal/db"
)

const ownsandRollbackCommand = "ownsand-rollback"

func runOwnSandRollback(args []string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result := ownSandRollback(ctx, args, strings.TrimSpace(os.Getenv(config.EnvDatabaseURL)))
	fmt.Println(result)
	switch result {
	case "open", "fenced", "ready":
		return 0
	default:
		return 1
	}
}

// ownSandRollback reports fixed outcomes. Database errors may contain
// connection details and must never reach deployment output.
func ownSandRollback(ctx context.Context, args []string, databaseURL string) string {
	if len(args) == 0 || databaseURL == "" {
		return "database-error"
	}
	database, err := db.Open(ctx, databaseURL)
	if err != nil {
		return "database-error"
	}
	defer database.Close()

	switch args[0] {
	case "status":
		fenced, err := database.OwnSandTeardownFenced(ctx)
		if err != nil {
			return "database-error"
		}
		if fenced {
			return "fenced"
		}
		return "open"
	case "fence":
		if len(args) != 2 {
			return "invalid-command"
		}
		err = database.FenceOwnSandTeardown(ctx, args[1])
		switch {
		case errors.Is(err, db.ErrOwnSandRemovalDrain):
			return "drain-incomplete"
		case errors.Is(err, db.ErrOwnSandRemovalTokens):
			return "tokens-remain"
		}
	case "cancel":
		if len(args) != 2 {
			return "invalid-command"
		}
		err = database.CancelOwnSandRollback(ctx, args[1])
	case "resume":
		err = database.ResumeOwnSandTeardown(ctx)
	default:
		return "invalid-command"
	}
	if err != nil {
		return "database-error"
	}
	return "ready"
}
