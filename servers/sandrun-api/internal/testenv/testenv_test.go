// SPDX-License-Identifier: FSL-1.1-ALv2

package testenv_test

import (
	"testing"

	"github.com/delinoio/mono/servers/sandrun-api/internal/testenv"
)

func TestFailProgramExitCommitsCanBeReenabled(t *testing.T) {
	env := testenv.New(t)
	assertFixtureInstalled := func(cycle int, installed bool) {
		t.Helper()
		var functionExists, triggerExists bool
		err := env.DB.Pool.QueryRow(t.Context(), `
			SELECT to_regprocedure('fail_program_exit()') IS NOT NULL,
			       EXISTS (
			           SELECT 1 FROM pg_trigger
			           WHERE tgrelid = 'runners'::regclass AND tgname = 'fail_program_exit'
			       )`).Scan(&functionExists, &triggerExists)
		if err != nil {
			t.Fatalf("cycle %d: query fixture objects: %v", cycle, err)
		}
		if functionExists != installed || triggerExists != installed {
			t.Fatalf("cycle %d: function exists = %t, trigger exists = %t, want both = %t",
				cycle, functionExists, triggerExists, installed)
		}
	}

	for cycle := 1; cycle <= 2; cycle++ {
		restore := env.FailProgramExitCommits()
		assertFixtureInstalled(cycle, true)
		restore()
		assertFixtureInstalled(cycle, false)
	}
}
