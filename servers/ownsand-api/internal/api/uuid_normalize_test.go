// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import "testing"

func TestNormalizeID(t *testing.T) {
	const lower = "01900000-0000-7abc-8def-0123456789ab"
	const upper = "01900000-0000-7ABC-8DEF-0123456789AB"
	for _, in := range []string{lower, upper} {
		got, err := normalizeID("id", in)
		if err != nil || got != lower {
			t.Fatalf("normalizeID(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "not-a-uuid", "01900000-0000-4abc-8def-0123456789ab"} {
		if _, err := normalizeID("id", bad); err == nil {
			t.Fatalf("normalizeID(%q) accepted", bad)
		}
	}
}

func TestAgentLimiterKey(t *testing.T) {
	const a = "01900000-0000-7abc-8def-0123456789ab"
	const b = "01900000-0000-7abc-8def-0123456789ac"
	for _, op := range []string{"challenge", "authenticate", "confirm-removal"} {
		upper, _ := normalizeID("device_id", "01900000-0000-7ABC-8DEF-0123456789AB")
		if agentLimiterKey(op, a) != agentLimiterKey(op, upper) {
			t.Fatalf("%s: equal ids give different keys", op)
		}
		if agentLimiterKey(op, a) == agentLimiterKey(op, b) {
			t.Fatalf("%s: different ids share a key", op)
		}
	}
	if agentLimiterKey("challenge", a) == agentLimiterKey("authenticate", a) {
		t.Fatal("operations share a namespace")
	}
}
