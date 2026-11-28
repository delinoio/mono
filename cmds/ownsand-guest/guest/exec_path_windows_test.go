// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// touch creates empty files in dir. lookPathIn only checks that a file
// exists, so the files need no executable content.
func touch(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLookPathInWindows(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "mytool.cmd", "mytool.exe", "plain.txt", "dotted.name.exe")
	if err := os.Mkdir(filepath.Join(dir, "adir.exe"), 0o700); err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()

	cases := []struct {
		name string
		prog string
		env  []string
		want string // file name in dir; empty means not found
	}{
		{"default PATHEXT order picks exe before cmd", "mytool", []string{"PATH=" + dir}, "mytool.exe"},
		{"PATHEXT order decides", "mytool", []string{"PATH=" + dir, "PATHEXT=.CMD;.EXE"}, "mytool.cmd"},
		{"PATHEXT without a dot", "mytool", []string{"PATH=" + dir, "PATHEXT=CMD"}, "mytool.cmd"},
		{"empty PATHEXT uses the default", "mytool", []string{"PATH=" + dir, "PATHEXT="}, "mytool.exe"},
		{"key spelling is folded", "mytool", []string{"Path=" + dir, "PathExt=.cmd"}, "mytool.cmd"},
		{"a name with an extension matches itself", "plain.txt", []string{"PATH=" + dir}, "plain.txt"},
		{"a dotted name still tries PATHEXT", "dotted.name", []string{"PATH=" + dir}, "dotted.name.exe"},
		{"a directory is no match", "adir", []string{"PATH=" + dir}, ""},
		{"extension outside PATHEXT", "mytool", []string{"PATH=" + dir, "PATHEXT=.BAT"}, ""},
		{"program not in PATH", "mytool", []string{"PATH=" + empty}, ""},
		{"no PATH", "mytool", []string{"PATHEXT=.EXE"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := lookPathIn(tc.prog, tc.env)
			if tc.want == "" {
				if !errors.Is(err, exec.ErrNotFound) {
					t.Fatalf("got %q, %v", got, err)
				}
				return
			}
			if err != nil || got != filepath.Join(dir, tc.want) {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}

	t.Run("implicit current directory match", func(t *testing.T) {
		t.Chdir(dir)
		other := t.TempDir()
		touch(t, other, "mytool.exe")
		alias := t.TempDir()
		if err := os.Link(filepath.Join(dir, "mytool.exe"), filepath.Join(alias, "mytool.exe")); err != nil {
			t.Fatal(err)
		}
		cases := []struct {
			name string
			path string
			want string // absolute path, or empty when the implicit match must return ErrDot
		}{
			{"same directory", dir, filepath.Join(dir, "mytool.exe")},
			{"same file through a hard link", alias, filepath.Join(alias, "mytool.exe")},
			{"empty PATH component", ";" + dir, filepath.Join(dir, "mytool.exe")},
			{"same relative match before absolute match", ".;" + dir, filepath.Join(dir, "mytool.exe")},
			{"no explicit match", empty, ""},
			{"only a relative match", ".", ""},
			{"different file before same file", other + ";" + dir, ""},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got, err := lookPathIn("mytool", []string{"PATH=" + tc.path, "PATHEXT=.EXE"})
				if tc.want == "" {
					if got != "mytool.exe" || !errors.Is(err, exec.ErrDot) {
						t.Fatalf("got %q, %v; want the implicit match with ErrDot", got, err)
					}
				} else if got != tc.want || err != nil {
					t.Fatalf("got %q, %v; want %q", got, err, tc.want)
				}
			})
		}
	})

	t.Run("relative request match without an implicit match", func(t *testing.T) {
		t.Chdir(empty)
		const sub = "bin"
		if err := os.Mkdir(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		touch(t, sub, "mytool.exe")
		if got, err := lookPathIn("mytool", []string{"PATH=" + sub}); got != filepath.Join(sub, "mytool.exe") || !errors.Is(err, exec.ErrDot) {
			t.Fatalf("got %q, %v; want the relative match with ErrDot", got, err)
		}
	})
}

func TestRequestSetsPathFoldsKeyCase(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"PATH", "Path", "path"} {
		if !requestSetsPath(map[string]string{key: `C:\x`}) {
			t.Errorf("key %q not recognized", key)
		}
	}
	if requestSetsPath(map[string]string{"PATHEXT": ".EXE", "MYPATH": "x"}) || requestSetsPath(nil) {
		t.Error("a request without PATH was recognized")
	}
}

// TestExecBareNameUsesRequestPathWindows copies the test binary to
// mytool.exe in a directory that the helper PATH does not hold and runs it
// by its bare name with a request "Path" (a spelling that differs from the
// "Path" or "PATH" of the helper environment).
func TestExecBareNameUsesRequestPathWindows(t *testing.T) {
	dir := t.TempDir()
	self, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer self.Close()
	out, err := os.OpenFile(filepath.Join(dir, "mytool.exe"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, self); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	helperPath := os.Getenv("PATH")
	h := newHarness(t, nil)

	req := helperExecReq(t, "exit", "", 30*time.Second)
	req.Command = []string{"mytool", "-test.run=^TestHelperProcess$"}
	// The system directories stay in the request PATH so that the copy
	// finds its libraries as the original does.
	req.Env["pAtH"] = dir + string(os.PathListSeparator) + helperPath
	evs := h.runExec(req)
	if err := requireExplicitSuccess(evs[len(evs)-1]); err != nil {
		t.Fatalf("events %v: %v", evs, err)
	}
	if os.Getenv("PATH") != helperPath {
		t.Fatalf("helper PATH changed to %q", os.Getenv("PATH"))
	}

	// Without a PATH in the request the helper PATH decides.
	req = helperExecReq(t, "exit", "", 30*time.Second)
	req.Command = []string{"mytool", "-test.run=^TestHelperProcess$"}
	evs = h.runExec(req)
	if got := evs[len(evs)-1].GetFailed().GetReason(); got != ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_PROGRAM_NOT_FOUND {
		t.Fatalf("events %v", evs)
	}

	t.Run("absolute request PATH names the helper current directory", func(t *testing.T) {
		t.Chdir(dir)
		req := helperExecReq(t, "exit", "", 30*time.Second)
		req.Command = []string{"mytool", "-test.run=^TestHelperProcess$"}
		req.Env["Path"] = ";" + dir + ";" + helperPath
		evs := h.runExec(req)
		if err := requireExplicitSuccess(evs[len(evs)-1]); err != nil {
			t.Fatalf("events %v: %v", evs, err)
		}
	})
}
