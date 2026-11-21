// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"io"
	"reflect"
	"testing"
)

func TestParseUntilTerminatorVersusFlagValue(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		stopAfter int
		want      []string
		wantOrg   string
		wantFmt   string
	}{
		{"flag value dash dash does not end flags", []string{"--org", "--", "pos", "--format", "json"}, -1, []string{"pos"}, "--", "json"},
		{"equals dash dash value", []string{"--org=--", "pos", "--format", "json"}, -1, []string{"pos"}, "--", "json"},
		{"real terminator keeps later flags positional", []string{"pos", "--", "--format", "json"}, -1, []string{"pos", "--format", "json"}, "", "text"},
		{"bool flag then terminator", []string{"--verbose", "--", "--format"}, -1, []string{"--format"}, "", "text"},
		{"value dash dash then terminator", []string{"--org", "--", "--", "--format"}, -1, []string{"--format"}, "--", "text"},
		{"stopAfter keeps terminator", []string{"id", "--", "ls", "--format"}, 2, []string{"id", "--", "ls", "--format"}, "", "text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &app{env: Env{LookupEnv: func(string) (string, bool) { return "", false }, Stderr: io.Discard}}
			a.g.format = "text"
			fs := a.newFlags("test")
			got, err := a.parseUntil(fs, tc.args, tc.stopAfter)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("positional = %v, want %v", got, tc.want)
			}
			if a.g.org != tc.wantOrg || a.g.format != tc.wantFmt {
				t.Errorf("org=%q format=%q, want %q %q", a.g.org, a.g.format, tc.wantOrg, tc.wantFmt)
			}
		})
	}
}
