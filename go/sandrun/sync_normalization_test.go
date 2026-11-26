// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
)

const (
	composedPath   = "caf\u00e9"
	decomposedPath = "cafe\u0301"
)

func needDistinctUnicodeNames(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, composedPath), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, decomposedPath)); err == nil {
		t.Skip("the local file system aliases canonically equivalent names")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestSyncNormalizationCollisions(t *testing.T) {
	needPOSIX(t)
	needDistinctUnicodeNames(t)
	c, srv, org := newTestClient(t, "")
	ctx := context.Background()
	const stale = "previous.txt"
	marks := strings.Repeat("\u0301", 30)
	longFirst, longSecond := "x"+marks+"\u0323", "x\u0323"+marks
	for _, tc := range []struct {
		name, first, second string
		paths               []string
		priorManifest       bool
	}{
		{"files", composedPath + ".txt", decomposedPath + ".txt", []string{decomposedPath + ".txt", composedPath + ".txt"}, false},
		{"prior manifest", composedPath + ".txt", decomposedPath + ".txt", []string{decomposedPath + ".txt", composedPath + ".txt"}, true},
		{"parents", composedPath + "/a.txt", decomposedPath + "/b.txt", []string{decomposedPath, composedPath}, false},
		{"file and parent", composedPath, decomposedPath + "/b.txt", []string{decomposedPath, composedPath}, false},
		{"case and normalization", "CAF\u00c9.txt", decomposedPath + ".txt", []string{"CAF\u00c9.txt", decomposedPath + ".txt"}, false},
		{"long combining sequence", longFirst, longSecond, []string{longFirst, longSecond}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds := newSession(t, c, srv, org, sandrunv1.GuestOs_GUEST_OS_MACOS)
			manifestPath := ds.GetWorkspacePath() + manifestSuffix
			params := SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), Force: true}
			var before []byte
			if tc.priorManifest {
				params.LocalDir = newRepo(t, map[string]string{stale: "keep"}, nil, "")
				if _, err := c.Sync(ctx, params); err != nil {
					t.Fatal(err)
				}
				var err error
				before, err = os.ReadFile(manifestPath)
				if err != nil {
					t.Fatal(err)
				}
			}
			writes, execs := srv.Calls(writeProc), srv.Calls(execProc)
			params.LocalDir = newRepo(t, map[string]string{tc.first: "first", tc.second: "second"}, nil, "")
			_, err := c.Sync(ctx, params)
			var pe *PathRuleError
			if connect.CodeOf(err) != connect.CodeFailedPrecondition || !errors.As(err, &pe) ||
				pe.Rule != PathRuleNormalizationCollision || pe.Rule.String() != "normalization_collision" ||
				len(pe.Paths) != 1 || !slices.Equal(pe.Paths[0], tc.paths) {
				t.Fatalf("normalization collision error = %v, want group %q", err, tc.paths)
			}
			for _, path := range tc.paths {
				if !strings.Contains(err.Error(), strconv.QuoteToASCII(path)) {
					t.Fatalf("error %q does not distinguish %q", err, path)
				}
			}
			if srv.Calls(writeProc) != writes || srv.Calls(execProc) != execs {
				t.Fatal("a rejected sync wrote guest files or ran guest commands")
			}
			after, err := os.ReadFile(manifestPath)
			if tc.priorManifest {
				if err != nil || !bytes.Equal(before, after) || readGuest(t, ds, stale) != "keep" {
					t.Fatalf("rejected sync changed the manifest or its files: %v", err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("manifest must be absent after rejection: %v", err)
			}
		})
	}
}

func TestSyncNormalizationKeepsOriginalPaths(t *testing.T) {
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	ctx := context.Background()
	const first, second = composedPath + ".txt", decomposedPath + ".txt"
	for _, guestOS := range []sandrunv1.GuestOs{
		sandrunv1.GuestOs_GUEST_OS_MACOS, sandrunv1.GuestOs_GUEST_OS_LINUX, sandrunv1.GuestOs_GUEST_OS_WINDOWS,
	} {
		t.Run(guestOS.String(), func(t *testing.T) {
			files := map[string]string{second: "second"}
			if guestOS != sandrunv1.GuestOs_GUEST_OS_MACOS {
				needDistinctUnicodeNames(t)
				files[first] = "first"
			}
			dir := newRepo(t, files, nil, "")
			if guestOS == sandrunv1.GuestOs_GUEST_OS_MACOS {
				// Git on macOS can precompose names before storing them in the index.
				path := strings.TrimSuffix(git(t, dir, "ls-files", "-z", "--cached"), "\x00")
				if path != first && path != second {
					t.Fatalf("expected one tracked Unicode path, got %+q", path)
				}
				files = map[string]string{path: "second"}
			}
			ds := newSession(t, c, srv, org, guestOS)
			params := SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir}
			if guestOS == sandrunv1.GuestOs_GUEST_OS_WINDOWS {
				// The fake runs on POSIX, so intercept the lock before it runs PowerShell.
				const lock = "intercepted Windows lock"
				srv.Fail(execProc, connect.NewError(connect.CodePermissionDenied, errors.New(lock)))
				_, err := c.Sync(ctx, params)
				if connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), lock) {
					t.Fatalf("Windows sync must reach the lock: %v", err)
				}
				return
			}
			result, err := c.Sync(ctx, params)
			if err != nil || result.Uploaded != len(files) {
				t.Fatalf("sync = %+v, %v", result, err)
			}
			m := guestManifest(t, ds)
			if len(m.Files) != len(files) {
				t.Fatalf("manifest = %+v", m)
			}
			for path, expected := range files {
				if m.Files[path].Mode != modeFile || readGuest(t, ds, path) != expected {
					t.Fatalf("sync did not preserve %+q and its contents", path)
				}
			}
		})
	}
}
