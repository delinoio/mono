// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package main

import (
	"slices"
	"strings"
	"testing"
)

// agentEnvPrefix starts the name of every environment variable of the agent.
const agentEnvPrefix = "OWNSAND_AGENT_"

// withoutAgentEnv returns env without any OWNSAND_AGENT_* entry, so that a
// subprocess of a test sees only the fixture values and never a setting
// inherited from the caller's shell. Other entries (PATH, HOME, ...) stay.
func withoutAgentEnv(env []string) []string {
	kept := make([]string, 0, len(env))
	for _, entry := range env {
		if strings.HasPrefix(entry, agentEnvPrefix) {
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}

func TestWithoutAgentEnv(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"OWNSAND_AGENT_LOG_LEVEL=invalid-fixture-level",
		"OWNSAND_AGENT_API_URL=http://127.0.0.1:1",
		"HOME=/home/test",
		"OWNSAND_AGENT_STATE_DIR=",
		"OWNSAND_OTHER=kept",
	}
	got := withoutAgentEnv(in)
	want := []string{"PATH=/usr/bin", "HOME=/home/test", "OWNSAND_OTHER=kept"}
	if !slices.Equal(got, want) {
		t.Fatalf("withoutAgentEnv = %v, want %v", got, want)
	}
	if len(in) != 6 {
		t.Fatalf("input slice changed: %v", in)
	}
}
