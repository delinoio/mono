// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
)

func TestSafeText(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"empty", "", ""},
		{"ASCII", "plain \\ text", "plain \\ text"},
		{"printable Unicode", "한국어 café 日本語　x 😀 �", "한국어 café 日本語　x 😀 �"},
		{"escape", "a\x1b[31mred\x1b[0m", "a\\x1b[31mred\\x1b[0m"},
		{"bell", "a\x07b", "a\\x07b"},
		{"carriage return", "a\rb", "a\\rb"},
		{"newline", "a\nb", "a\\nb"},
		{"tab", "a\tb", "a\\tb"},
		{"NUL and DEL", "\x00\x7f", "\\x00\\x7f"},
		{"C1 control", "a\u009bb", "a\\x9bb"},
		{"line and paragraph separators", "a\u2028b\u2029c", "a\\u2028b\\u2029c"},
		{"bidirectional override", "a\u202eb", "a\\u202eb"},
		{"unassigned code point", "a\u0378b", "a\\u0378b"},
		{"supplementary nonprintable rune", "a\U000e0001b", "a\\U000e0001b"},
		{"invalid UTF-8", "a\xffb\xc3", "a\\xffb\\xc3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := safeText(tc.in); got != tc.want {
				t.Fatalf("safeText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPrintFieldsEscapesControlCharacters(t *testing.T) {
	var buf bytes.Buffer
	a := &app{}
	m := &sandrunv1.DevSession{DevSessionId: "id", Label: "label\x1b[0m\nsecond line: sample\t한국어"}
	if err := a.printFields(&buf, m); err != nil {
		t.Fatal(err)
	}
	want := "dev_session_id: id\nlabel: label\\x1b[0m\\nsecond line: sample\\t한국어\n"
	if got := buf.String(); got != want {
		t.Fatalf("fields = %q, want %q", got, want)
	}

	buf.Reset()
	a.color = true
	fields, err := structpb.NewStruct(map[string]any{"key\x1b\n": "value\r"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.printFields(&buf, fields); err != nil {
		t.Fatal(err)
	}
	want = ansiCyan + "key\\x1b\\n" + ansiReset + ": value\\r\n"
	if got := buf.String(); got != want {
		t.Fatalf("colored fields = %q, want %q", got, want)
	}
}

func TestPrintTableEscapesCells(t *testing.T) {
	var buf bytes.Buffer
	a := &app{}
	rows := [][]string{{"1", "a\nb\tc\x1b[0m", "x"}, {"2", "plain 한국어", "y"}}
	if err := a.printTable(&buf, []string{"ID", "NA\tME", "Z\xff"}, rows); err != nil {
		t.Fatal(err)
	}
	want := "ID  NA\\tME          Z\\xff\n" +
		"1   a\\nb\\tc\\x1b[0m  x\n" +
		"2   plain 한국어       y\n"
	if got := buf.String(); got != want {
		t.Fatalf("table = %q, want %q", got, want)
	}
}

func TestSessionTextOutputEscapesLabels(t *testing.T) {
	f := newFixture(t)
	f.srv.AddDevSessionProfile(f.org, "linux", sandrunv1.GuestOs_GUEST_OS_LINUX, f.wd)
	f.env[EnvOrganization] = f.org
	const label = "café\u202e\u2028\u2029\nlabel\ttext"
	const escaped = "café\\u202e\\u2028\\u2029\\nlabel\\ttext"
	created := f.ok("", "session", "create", "linux", "--label", label)
	if !strings.Contains(created.stdout, "\nlabel: "+escaped+"\n") {
		t.Fatalf("session create stdout = %q", created.stdout)
	}

	listed := f.ok("", "session", "list")
	if strings.Count(listed.stdout, "\n") != 2 || !strings.Contains(listed.stdout, escaped) {
		t.Fatalf("session list stdout = %q", listed.stdout)
	}
	var response sandrunv1.ListDevSessionsResponse
	listed = f.ok("", "--output", "json", "session", "list")
	if err := protojson.Unmarshal([]byte(listed.stdout), &response); err != nil {
		t.Fatal(err)
	}
	if sessions := response.GetDevSessions(); len(sessions) != 1 || sessions[0].GetLabel() != label {
		t.Fatalf("session list JSON = %q", listed.stdout)
	}
}

func TestPrintSecretPreservesRawStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := &app{env: Env{Stdout: &stdout, Stderr: &stderr}}
	const secret = "sample\tsecret\xff"
	if err := a.printSecret(&sandrunv1.ApiKey{Name: "sample\nname"}, "secret", secret); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != secret+"\n" {
		t.Fatalf("secret stdout = %q, want %q", got, secret+"\n")
	}
	if got := stderr.String(); !strings.HasPrefix(got, "name: sample\\nname\n") {
		t.Fatalf("secret metadata stderr = %q", got)
	}
}
