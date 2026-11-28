// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"io"
	"reflect"
	"testing"
)

func TestParseUntilTerminatorVersusFlagValue(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		stopAfter  int
		want       []string
		wantOrg    string
		wantFmt    string
		terminated bool
	}{
		{"flag value dash dash does not end flags", []string{"--org", "--", "pos", "--format", "json"}, -1, []string{"pos"}, "--", "json", false},
		{"equals dash dash value", []string{"--org=--", "pos", "--format", "json"}, -1, []string{"pos"}, "--", "json", false},
		{"real terminator keeps later flags positional", []string{"pos", "--", "--format", "json"}, -1, []string{"pos", "--format", "json"}, "", "text", true},
		{"bool flag then terminator", []string{"--verbose", "--", "--format"}, -1, []string{"--format"}, "", "text", true},
		{"value dash dash then terminator", []string{"--org", "--", "--", "--format"}, -1, []string{"--format"}, "--", "text", true},
		{"stopAfter keeps terminator", []string{"id", "--", "ls", "--format"}, 2, []string{"id", "--", "ls", "--format"}, "", "text", true},
		{"stopAfter leaves a later terminator for the handler", []string{"org", "list", "--"}, 1, []string{"org", "list", "--"}, "", "text", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &app{env: Env{LookupEnv: func(string) (string, bool) { return "", false }, Stderr: io.Discard}}
			a.g.format = "text"
			fs := a.newFlags("test")
			got, terminated, err := a.parseUntil(fs, tc.args, tc.stopAfter)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("positional = %v, want %v", got, tc.want)
			}
			if terminated != tc.terminated {
				t.Errorf("terminated = %t, want %t", terminated, tc.terminated)
			}
			if a.g.org != tc.wantOrg || a.g.format != tc.wantFmt {
				t.Errorf("org=%q format=%q, want %q %q", a.g.org, a.g.format, tc.wantOrg, tc.wantFmt)
			}
		})
	}
}
