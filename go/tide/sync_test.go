// SPDX-License-Identifier: Apache-2.0

package tide

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/tide/tidetest"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/protos/delino/tide/v1/tidev1connect"
)

const writeProc = "/delino.tide.v1.DevSessionService/WriteDevSessionFile"
const execProc = "/delino.tide.v1.DevSessionService/ExecDevSession"

// newRepo creates a Git working tree with the given files (content by
// slash path) and commits the tracked ones.
func newRepo(t *testing.T, tracked, untracked map[string]string, gitignore string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	write := func(files map[string]string) {
		for p, content := range files {
			full := filepath.Join(dir, filepath.FromSlash(p))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	write(tracked)
	if gitignore != "" {
		write(map[string]string{".gitignore": gitignore})
	}
	git(t, dir, "add", "-A")
	git(t, dir, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "-m", "init", "--allow-empty")
	write(untracked)
	return dir
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func readGuest(t *testing.T, ds *tidev1.DevSession, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(ds.GetWorkspacePath(), rel))
	if err != nil {
		t.Fatalf("guest file %s: %v", rel, err)
	}
	return string(b)
}

func guestManifest(t *testing.T, ds *tidev1.DevSession) manifest {
	t.Helper()
	b, err := os.ReadFile(ds.GetWorkspacePath() + manifestSuffix)
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestTransferWithoutWorkspace(t *testing.T) {
	guestOS := tidev1.GuestOs_GUEST_OS_LINUX
	switch runtime.GOOS {
	case "windows":
		guestOS = tidev1.GuestOs_GUEST_OS_WINDOWS
	case "darwin":
		guestOS = tidev1.GuestOs_GUEST_OS_MACOS
	}
	for _, tc := range []struct {
		name           string
		force          bool
		removeManifest bool
		breakArchive   bool
	}{
		{name: "prior manifest"},
		{name: "force", force: true},
		{name: "manifest removed", removeManifest: true},
		{name: "force and manifest removed", force: true, removeManifest: true},
		{name: "retry after apply fails", breakArchive: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, srv, org := newTestClient(t, "")
			ds := newSession(t, c, srv, org, guestOS)
			ws := ds.GetWorkspacePath()
			dir := newRepo(t, map[string]string{"a.txt": "unchanged"}, nil, "")
			params := SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir}
			if _, err := c.Sync(t.Context(), params); err != nil {
				t.Fatal(err)
			}
			prior := guestManifest(t, ds)
			if err := os.RemoveAll(ws); err != nil {
				t.Fatal(err)
			}
			if tc.removeManifest {
				if err := os.Remove(ws + manifestSuffix); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("added"), 0o644); err != nil {
				t.Fatal(err)
			}
			params.Force = tc.force
			if tc.breakArchive {
				srv.BeforeExec = func(req *tidev1.ExecDevSessionRequest) {
					env := req.GetEnv()
					if archive := env[guestEnvArchive]; archive != "" && env[guestEnvWorkspace] != "" {
						if err := os.WriteFile(archive, []byte("invalid tar"), 0o644); err != nil {
							t.Error(err)
						}
					}
				}
				_, err := c.Sync(t.Context(), params)
				srv.BeforeExec = nil
				if connect.CodeOf(err) != connect.CodeInternal {
					t.Fatalf("sync with corrupt archive = %v, want apply failure", err)
				}
				if info, err := os.Stat(ws); err != nil || !info.IsDir() {
					t.Fatalf("workspace should exist after failed apply: %v", err)
				}
				if _, err := os.Stat(ws + manifestSuffix); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("stale manifest survived failed apply: %v", err)
				}
			}
			res, err := c.Sync(t.Context(), params)
			if err != nil || res.Uploaded != 2 || res.Skipped != 0 {
				t.Fatalf("restoring the workspace = %+v, %v", res, err)
			}
			if a, b := readGuest(t, ds, "a.txt"), readGuest(t, ds, "b.txt"); a != "unchanged" || b != "added" {
				t.Fatalf("restored files = %q and %q", a, b)
			}
			m := guestManifest(t, ds)
			bHash := sha256.Sum256([]byte("added"))
			if len(m.Files) != 2 || m.Files["a.txt"] != prior.Files["a.txt"] ||
				m.Files["b.txt"] != (manifestEntry{Size: 5, SHA256: hex.EncodeToString(bHash[:]), Mode: modeFile}) {
				t.Fatalf("restored manifest = %+v", m)
			}
			requireTransferWorkingDirectories(t, ds, srv)
		})
	}

	t.Run("download", func(t *testing.T) {
		c, srv, org := newTestClient(t, "")
		ds := newSession(t, c, srv, org, guestOS)
		ws := ds.GetWorkspacePath()
		if err := os.RemoveAll(ws); err != nil {
			t.Fatal(err)
		}
		local := t.TempDir()
		for _, p := range []string{"missing.txt", filepath.Join(ws, "missing.txt")} {
			_, err := c.Download(t.Context(), DownloadParams{
				OrganizationID: org, DevSessionID: ds.GetDevSessionId(), GuestPath: p,
				LocalPath: filepath.Join(local, "missing.txt"),
			})
			if connect.CodeOf(err) != connect.CodeNotFound {
				t.Fatalf("download of %q = %v, want not_found", p, err)
			}
		}

		outside := t.TempDir()
		file := filepath.Join(outside, "result.txt")
		if err := os.WriteFile(file, []byte("built"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			guestPath string
			dest      string
			file      string
		}{
			{guestPath: file, dest: "file", file: "file"},
			{guestPath: outside, dest: "directory", file: filepath.Join("directory", "result.txt")},
		} {
			_, err := c.Download(t.Context(), DownloadParams{
				OrganizationID: org, DevSessionID: ds.GetDevSessionId(), GuestPath: tc.guestPath,
				LocalPath: filepath.Join(local, tc.dest),
			})
			if err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(filepath.Join(local, tc.file)); err != nil || string(got) != "built" {
				t.Fatalf("downloaded file = %q, %v", got, err)
			}
		}
		requireTransferWorkingDirectories(t, ds, srv)

		// A user command with no working directory still receives the workspace.
		if err := os.Mkdir(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		command := []string{"pwd"}
		if guestOS == tidev1.GuestOs_GUEST_OS_WINDOWS {
			command = []string{"cmd.exe", "/d", "/c", "cd"}
		}
		var output collected
		res, err := c.Exec(t.Context(), ExecParams{
			OrganizationID: org, DevSessionID: ds.GetDevSessionId(), Command: command, OnEvent: output.on,
		})
		if err != nil || res.ExitCode == nil || *res.ExitCode != 0 {
			t.Fatalf("user command = %+v, %v", res, err)
		}
		reqs := srv.ExecRequests()
		if got := reqs[len(reqs)-1].GetWorkingDirectory(); got != "" {
			t.Fatalf("user working directory in request = %q, want empty", got)
		}
		dir, err := os.Stat(strings.TrimSpace(output.stdout.String()))
		want, wantErr := os.Stat(ws)
		if err != nil || wantErr != nil || !os.SameFile(dir, want) {
			t.Fatalf("user command ran in %q, want %q", output.stdout.String(), ws)
		}
	})

	t.Run("missing workspace parent", func(t *testing.T) {
		c, srv, org := newTestClient(t, "")
		ds := newSession(t, c, srv, org, guestOS)
		parent := filepath.Dir(ds.GetWorkspacePath())
		if err := os.RemoveAll(parent); err != nil {
			t.Fatal(err)
		}
		dir := newRepo(t, map[string]string{"a.txt": "a"}, nil, "")
		_, err := c.Sync(t.Context(), SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir})
		if connect.CodeOf(err) != connect.CodeInternal {
			t.Fatalf("sync without workspace parent = %v, want lock failure", err)
		}
		if _, err := os.Stat(parent); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("sync recreated the workspace parent: %v", err)
		}
		requireTransferWorkingDirectories(t, ds, srv)
	})
}

func requireTransferWorkingDirectories(t *testing.T, ds *tidev1.DevSession, srv *tidetest.Server) {
	t.Helper()
	want := "/"
	if ds.GetGuestOs() == tidev1.GuestOs_GUEST_OS_WINDOWS {
		want = filepath.VolumeName(ds.GetWorkspacePath()) + `\`
	}
	reqs := srv.ExecRequests()
	if len(reqs) == 0 {
		t.Fatal("transfer sent no guest exec requests")
	}
	for _, req := range reqs {
		if got := req.GetWorkingDirectory(); got != want {
			t.Fatalf("transfer working directory = %q, want %q", got, want)
		}
	}
}

func TestSyncFileSelectionAndIncrementalChanges(t *testing.T) {
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	ds := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_LINUX)
	dir := newRepo(t,
		map[string]string{"a.txt": "a", "src/main.go": "package main", "old.txt": "old", "run.sh": "#!/bin/sh\necho hi\n"},
		map[string]string{"new.txt": "untracked", "build/out.bin": "ignored", "debug.log": "ignored"},
		"build/\n*.log\n")
	if err := os.Chmod(filepath.Join(dir, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("src/main.go", filepath.Join(dir, "link.go")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var phases []TransferPhase
	res, err := c.Sync(ctx, SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir, Progress: func(p TransferProgress) {
		if len(phases) == 0 || phases[len(phases)-1] != p.Phase {
			phases = append(phases, p.Phase)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	// .gitignore, a.txt, link.go, new.txt, old.txt, run.sh, src/main.go
	if res.Uploaded != 7 || res.Deleted != 0 || res.Skipped != 0 || res.BytesSent == 0 {
		t.Fatalf("first sync = %+v", res)
	}
	if !slices.Contains(phases, TransferUploading) || phases[len(phases)-1] != TransferDone {
		t.Fatalf("phases %v", phases)
	}
	if readGuest(t, ds, "new.txt") != "untracked" || readGuest(t, ds, "src/main.go") != "package main" {
		t.Fatal("guest content differs")
	}
	for _, ignored := range []string{"build/out.bin", "debug.log"} {
		if _, err := os.Stat(filepath.Join(ds.GetWorkspacePath(), ignored)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ignored file %s was synced", ignored)
		}
	}
	if info, err := os.Stat(filepath.Join(ds.GetWorkspacePath(), "run.sh")); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("executable bit lost: %v %v", info.Mode(), err)
	}
	if target, err := os.Readlink(filepath.Join(ds.GetWorkspacePath(), "link.go")); err != nil || target != "src/main.go" {
		t.Fatalf("symbolic link: %q %v", target, err)
	}
	m := guestManifest(t, ds)
	if len(m.Files) != 7 || m.Files["run.sh"].Mode != modeExecutable || m.Files["link.go"].Mode != modeSymlink {
		t.Fatalf("manifest %+v", m)
	}
	for _, leftover := range []string{lockSuffix} {
		if _, err := os.Stat(ds.GetWorkspacePath() + leftover); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left behind", leftover)
		}
	}
	matches, _ := filepath.Glob(ds.GetWorkspacePath() + ".tide-sync-*")
	if len(matches) != 0 {
		t.Fatalf("temporary files left: %v", matches)
	}

	// A guest-created file stays; a change, a deletion and a mode change
	// are applied incrementally.
	if err := os.WriteFile(filepath.Join(ds.GetWorkspacePath(), "guest-output.txt"), []byte("built"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "rm", "-q", "old.txt")
	writesBefore := srv.Calls(writeProc)
	res, err = c.Sync(ctx, SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if res.Uploaded != 1 || res.Deleted != 1 || res.Skipped != 5 {
		t.Fatalf("second sync = %+v", res)
	}
	if readGuest(t, ds, "a.txt") != "changed" || readGuest(t, ds, "guest-output.txt") != "built" {
		t.Fatal("incremental content wrong")
	}
	if _, err := os.Stat(filepath.Join(ds.GetWorkspacePath(), "old.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deleted file stayed")
	}
	// archive, deletion list and manifest
	if n := srv.Calls(writeProc) - writesBefore; n != 3 {
		t.Fatalf("writes of the second sync = %d, want 3", n)
	}

	// An unchanged sync takes and releases the lock without writing files.
	execsBefore := srv.Calls(execProc)
	writesBefore = srv.Calls(writeProc)
	res, err = c.Sync(ctx, SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir})
	if err != nil || res.Uploaded != 0 || res.Deleted != 0 || res.Skipped != 6 || res.BytesSent != 0 || len(res.Warnings) != 0 {
		t.Fatalf("unchanged sync = %+v %v", res, err)
	}
	if n := srv.Calls(execProc) - execsBefore; n != 2 {
		t.Fatalf("guest commands of unchanged sync = %d, want lock and cleanup", n)
	}
	if srv.Calls(writeProc) != writesBefore {
		t.Fatal("unchanged sync wrote to the guest")
	}
	if _, err := os.Stat(ds.GetWorkspacePath() + lockSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock after unchanged sync: %v", err)
	}

	// A subdirectory of the working tree syncs only its own files.
	sub := filepath.Join(dir, "src")
	ds2 := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_LINUX)
	res, err = c.Sync(ctx, SyncParams{OrganizationID: org, DevSessionID: ds2.GetDevSessionId(), LocalDir: sub})
	if err != nil || res.Uploaded != 1 || readGuest(t, ds2, "main.go") != "package main" {
		t.Fatalf("subdirectory sync = %+v %v", res, err)
	}
}

func TestSyncSkipsStaleSymlinkDescendants(t *testing.T) {
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	ctx := context.Background()
	const (
		replacement = "src/former"
		descendant  = replacement + "/inner/file.txt"
		target      = "../target"
		targetFile  = "target/inner/file.txt"
	)
	for _, tc := range []struct {
		name          string
		targetContent string
		ignored       bool
	}{
		{name: "matching target content", targetContent: "old"},
		{name: "different target content", targetContent: "new"},
		{name: "ignored link", targetContent: "old", ignored: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := newRepo(t, map[string]string{
				descendant:     "old",
				targetFile:     tc.targetContent,
				"src/keep.txt": "keep",
			}, nil, "")
			if err := os.Symlink(targetFile, filepath.Join(dir, "standalone")); err != nil {
				t.Fatal(err)
			}
			ds := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_LINUX)
			params := SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir}
			if _, err := c.Sync(ctx, params); err != nil {
				t.Fatal(err)
			}
			old := guestManifest(t, ds)
			indexPath := filepath.Join(dir, ".git", "index")
			indexBefore, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatal(err)
			}

			if tc.ignored {
				exclude := filepath.Join(dir, ".git", "info", "exclude")
				if err := os.WriteFile(exclude, []byte("/"+replacement+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			link := filepath.Join(dir, replacement)
			if err := os.RemoveAll(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}

			local, err := listLocalFiles(ctx, dir)
			if err != nil {
				t.Fatal(err)
			}
			selected := make(map[string]localFile, len(local.files))
			for _, f := range local.files {
				selected[f.rel] = f
			}
			if _, ok := selected[descendant]; ok {
				t.Fatalf("selection includes stale path %q", descendant)
			}
			for _, rel := range []string{"src/keep.txt", targetFile, "standalone"} {
				if f, ok := selected[rel]; !ok || f.entry != old.Files[rel] {
					t.Fatalf("selection changed %q: %+v", rel, f)
				}
			}
			f, hasLink := selected[replacement]
			if tc.ignored && hasLink {
				t.Fatalf("selection includes ignored link %q", replacement)
			}
			if !tc.ignored && (!hasLink || f.entry.Mode != modeSymlink || f.target != target) {
				t.Fatalf("selection for %q = %+v", replacement, f)
			}

			plan, err := planSync(local, old)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(plan.deleted, []string{descendant}) {
				t.Fatalf("planned deletions = %v, want [%s]", plan.deleted, descendant)
			}
			var wantChanges []localFile
			if !tc.ignored {
				wantChanges = []localFile{f}
			}
			if !slices.Equal(plan.changed, wantChanges) {
				t.Fatalf("planned changes = %+v, want %+v", plan.changed, wantChanges)
			}

			res, err := c.Sync(ctx, params)
			if err != nil {
				t.Fatal(err)
			}
			if res.Uploaded != len(wantChanges) || res.Deleted != 1 {
				t.Fatalf("second sync = %+v", res)
			}
			m := guestManifest(t, ds)
			if _, ok := m.Files[descendant]; ok {
				t.Fatalf("guest manifest still includes %q", descendant)
			}
			if _, ok := m.Files[replacement]; ok != hasLink {
				t.Fatalf("guest manifest link %q present = %t, want %t", replacement, ok, hasLink)
			}
			if hasLink {
				if got, err := os.Readlink(filepath.Join(ds.GetWorkspacePath(), replacement)); err != nil || got != target {
					t.Fatalf("guest symbolic link = %q, %v", got, err)
				}
			}
			if got := readGuest(t, ds, targetFile); got != tc.targetContent {
				t.Fatalf("guest target content = %q, want %q", got, tc.targetContent)
			}
			indexAfter, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(indexBefore, indexAfter) {
				t.Fatal("sync changed the Git index")
			}
		})
	}
}

func TestSyncSkipsSubmodulesAndNeedsGit(t *testing.T) {
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	ds := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_LINUX)
	dir := newRepo(t, map[string]string{"a.txt": "a"}, nil, "")
	git(t, dir, "update-index", "--add", "--cacheinfo", "160000,0123456789012345678901234567890123456789,vendor/lib")
	res, err := c.Sync(context.Background(), SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if res.Uploaded != 1 || !slices.Equal(res.Submodules, []string{"vendor/lib"}) || res.Skipped != 1 {
		t.Fatalf("submodule sync = %+v", res)
	}

	plain := t.TempDir()
	ceiling, err := filepath.Abs(filepath.Dir(plain))
	if err != nil {
		t.Fatal(err)
	}
	ceiling, err = filepath.EvalSymlinks(ceiling)
	if err != nil {
		t.Fatal(err)
	}
	// Stop Git from finding the enclosing checkout when TMPDIR is inside it.
	t.Setenv("GIT_CEILING_DIRECTORIES", ceiling)
	_, err = c.Sync(context.Background(), SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: plain})
	if !errors.Is(err, ErrNotGitWorkTree) || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("not a work tree: %v", err)
	}
}

func TestSyncLimits(t *testing.T) {
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	ds := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_LINUX)
	dir := newRepo(t, map[string]string{"small.txt": "s"}, nil, "")
	big := filepath.Join(dir, "big.bin")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(SyncFileLimit + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	_, err = c.Sync(context.Background(), SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir})
	var le *LimitError
	if !errors.As(err, &le) || connect.CodeOf(err) != connect.CodeFailedPrecondition || len(le.Paths) != 1 || le.Paths[0].Path != "big.bin" || le.Paths[0].Size != SyncFileLimit+1 {
		t.Fatalf("limit error = %v", err)
	}
	if !strings.Contains(err.Error(), "big.bin") {
		t.Fatalf("message %q lacks the path", err.Error())
	}
	if srv.Calls(writeProc) != 0 || srv.Calls(execProc) != 0 {
		t.Fatal("a sync over the limit uploaded or ran guest commands")
	}
}

func TestSyncManifestLimitCandidate(t *testing.T) {
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	ds := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_LINUX)
	dir := newRepo(t, map[string]string{"old.txt": "old"}, nil, "")
	ctx := context.Background()
	params := SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir}
	if _, err := c.Sync(ctx, params); err != nil {
		t.Fatal(err)
	}
	ws := ds.GetWorkspacePath()
	previous, err := os.ReadFile(ws + manifestSuffix)
	if err != nil {
		t.Fatal(err)
	}

	git(t, dir, "rm", "-q", "old.txt")
	// The JSON key for a&b.txt contains a Unicode escape. Count encoded bytes.
	entries := make(map[string]manifestEntry)
	for name, contents := range map[string]string{"a&b.txt": "a", "new.txt": "new"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(contents))
		entries[name] = manifestEntry{Size: int64(len(contents)), SHA256: hex.EncodeToString(sum[:]), Mode: modeFile}
	}
	want, err := json.Marshal(manifest{Version: manifestVersion, Files: entries, PathsValidated: true})
	if err != nil {
		t.Fatal(err)
	}

	writesBefore, execsBefore := srv.Calls(writeProc), srv.Calls(execProc)
	_, err = c.sync(ctx, params, int64(len(want)-1))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "candidate manifest exceeds the size limit") {
		t.Fatalf("oversized candidate manifest: %v", err)
	}
	if srv.Calls(writeProc) != writesBefore || srv.Calls(execProc) != execsBefore {
		t.Fatal("oversized candidate manifest caused an upload or a guest command")
	}
	if got, err := os.ReadFile(ws + manifestSuffix); err != nil || !bytes.Equal(got, previous) {
		t.Fatalf("previous manifest changed: %q, %v", got, err)
	}
	if got := readGuest(t, ds, "old.txt"); got != "old" {
		t.Fatalf("previous guest file changed: %q", got)
	}

	res, err := c.sync(ctx, params, int64(len(want)))
	if err != nil || res.Uploaded != 2 || res.Deleted != 1 {
		t.Fatalf("sync at the limit = %+v, %v", res, err)
	}
	if got, err := os.ReadFile(ws + manifestSuffix); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("manifest at the limit = %q, %v", got, err)
	}
	res, err = c.sync(ctx, params, int64(len(want)))
	if err != nil || res.Uploaded != 0 || res.Skipped != 2 {
		t.Fatalf("incremental sync at the limit = %+v, %v", res, err)
	}
}

func TestSyncManifestLimitGuest(t *testing.T) {
	needPOSIX(t)
	for _, name := range []string{"initial read", "read under the lock"} {
		t.Run(name, func(t *testing.T) {
			c, srv, org := newTestClient(t, "")
			ds := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_LINUX)
			dir := newRepo(t, map[string]string{"a.txt": "local"}, nil, "")
			ctx := context.Background()
			ws := ds.GetWorkspacePath()
			guestFile := filepath.Join(ws, "a.txt")
			if err := os.WriteFile(guestFile, []byte("guest"), 0o644); err != nil {
				t.Fatal(err)
			}
			const limit = 200
			body := []byte(`{"version":1,"files":{}}`)
			atLimit := append(bytes.Clone(body), bytes.Repeat([]byte(" "), limit-len(body))...)
			oversized := append(bytes.Clone(atLimit), ' ')
			start := oversized
			wantExecs := 0
			if name == "read under the lock" {
				start = atLimit
				wantExecs = 2 // lock and cleanup
				srv.BeforeExec = func(req *tidev1.ExecDevSessionRequest) {
					if req.GetEnv()[guestEnvForce] != "" {
						// Another sync replaces the manifest before this sync takes the lock.
						if err := os.WriteFile(ws+manifestSuffix, oversized, 0o644); err != nil {
							t.Error(err)
						}
					}
				}
			}
			if err := os.WriteFile(ws+manifestSuffix, start, 0o644); err != nil {
				t.Fatal(err)
			}
			res, err := c.sync(ctx, SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir}, limit)
			if res != nil || connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "guest manifest exceeds the size limit") {
				t.Fatalf("oversized guest manifest = %+v, %v", res, err)
			}
			if writes, execs := srv.Calls(writeProc), srv.Calls(execProc); writes != 0 || execs != wantExecs {
				t.Fatalf("got %d uploads and %d execs, want 0 uploads and %d execs", writes, execs, wantExecs)
			}
			if got, err := os.ReadFile(ws + manifestSuffix); err != nil || !bytes.Equal(got, oversized) {
				t.Fatalf("oversized manifest changed: %q, %v", got, err)
			}
			if got := readGuest(t, ds, "a.txt"); got != "guest" {
				t.Fatalf("guest file changed: %q", got)
			}
			if _, err := os.Stat(ws + lockSuffix); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("sync lock after failure: %v", err)
			}
		})
	}
}

func TestSyncCaseCollisionsAndWindowsNames(t *testing.T) {
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	ctx := context.Background()
	// The colliding paths exist only on a case-sensitive local file system.
	// On a case-insensitive one (the default on macOS) the second write
	// replaces the first file, so there is no collision to detect.
	if !caseSensitiveDir(t, t.TempDir()) {
		t.Log("the local file system ignores case; the case collision checks are skipped")
	} else {
		testSyncCaseCollisions(t, c, srv, org)
	}

	writes := srv.Calls(writeProc)
	invalid := newRepo(t, map[string]string{"con.txt": "x", "a:b.txt": "x", "trailing. ": "x", "ok.txt": "x", "dir/AUX": "x"}, nil, "")
	win := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_WINDOWS)
	_, err := c.Sync(ctx, SyncParams{OrganizationID: org, DevSessionID: win.GetDevSessionId(), LocalDir: invalid})
	var pe *PathRuleError
	if !errors.As(err, &pe) || pe.Rule != PathRuleInvalidWindowsName || len(pe.Paths) != 4 {
		t.Fatalf("invalid names error = %v", err)
	}
	if srv.Calls(writeProc) != writes {
		t.Fatal("a rejected sync uploaded")
	}
}

func TestSyncInvalidUTF8(t *testing.T) {
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	dir := newRepo(t, map[string]string{"ok.txt": "ok"}, nil, "")
	const bad = "bad\xffname.txt"
	badPath := filepath.Join(dir, bad)
	if err := os.WriteFile(badPath, []byte("new"), 0o644); err != nil {
		if errors.Is(err, fs.ErrInvalid) || errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.EILSEQ) {
			t.Skipf("the local file system rejected a non-UTF-8 file name: %v", err)
		}
		t.Fatal(err)
	}
	if got := PathRuleInvalidUTF8.String(); got != "invalid_utf8" {
		t.Fatalf("rule identifier = %q", got)
	}
	linux := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_LINUX)
	for _, guestOS := range []tidev1.GuestOs{
		tidev1.GuestOs_GUEST_OS_LINUX, tidev1.GuestOs_GUEST_OS_MACOS, tidev1.GuestOs_GUEST_OS_WINDOWS,
	} {
		t.Run(guestOS.String(), func(t *testing.T) {
			ds := linux
			if guestOS != tidev1.GuestOs_GUEST_OS_LINUX {
				ds = newSession(t, c, srv, org, guestOS)
			}
			reads, writes, execs := len(srv.ReadRequests()), srv.Calls(writeProc), srv.Calls(execProc)
			_, err := c.Sync(context.Background(), SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir, Force: true})
			var pe *PathRuleError
			if connect.CodeOf(err) != connect.CodeFailedPrecondition || !errors.As(err, &pe) ||
				pe.Rule != PathRuleInvalidUTF8 || len(pe.Paths) != 1 || !slices.Equal(pe.Paths[0], []string{bad}) {
				t.Fatalf("invalid UTF-8 error = %v, path error = %+v", err, pe)
			}
			if msg := err.Error(); !utf8.ValidString(msg) || !strings.Contains(msg, strconv.Quote(bad)) {
				t.Fatalf("error must quote the path and contain valid UTF-8: %q", msg)
			}
			if len(srv.ReadRequests()) != reads || srv.Calls(writeProc) != writes || srv.Calls(execProc) != execs {
				t.Fatal("a rejected sync read or wrote files or ran guest commands")
			}
			if _, err := os.Stat(ds.GetWorkspacePath() + manifestSuffix); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("manifest must be absent after rejection: %v", err)
			}
		})
	}

	const valid = "ü.txt"
	if err := os.Rename(badPath, filepath.Join(dir, valid)); err != nil {
		t.Fatal(err)
	}
	params := SyncParams{OrganizationID: org, DevSessionID: linux.GetDevSessionId(), LocalDir: dir}
	res, err := c.Sync(context.Background(), params)
	if err != nil || res.Uploaded != 2 || res.Deleted != 0 || res.Skipped != 0 {
		t.Fatalf("sync after rename = %+v, %v", res, err)
	}
	writes, execs := srv.Calls(writeProc), srv.Calls(execProc)
	for range 2 {
		res, err = c.Sync(context.Background(), params)
		if err != nil || res.Uploaded != 0 || res.Deleted != 0 || res.Skipped != 2 || res.BytesSent != 0 {
			t.Fatalf("unchanged sync = %+v, %v", res, err)
		}
	}
	if srv.Calls(writeProc) != writes || srv.Calls(execProc)-execs != 4 {
		t.Fatal("each unchanged sync must only run lock and cleanup commands without writing files")
	}
	if m := guestManifest(t, linux); !m.PathsValidated || m.Files[valid].Mode != modeFile {
		t.Fatalf("manifest did not record the valid path %q: %+v", valid, m)
	}
}

func TestSyncLegacyAmbiguousUTF8Manifest(t *testing.T) {
	needPOSIX(t)
	for _, scenario := range []struct {
		name              string
		localHasPath      bool
		clearMarkerOnLock bool
	}{
		{name: "drop stale entry without applying"},
		{name: "replan when only the validation marker changes", clearMarkerOnLock: true},
		{name: "reupload matching local file before managing it", localHasPath: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			c, srv, org := newTestClient(t, "")
			ds := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_LINUX)
			ws := ds.GetWorkspacePath()
			const raw = "entry-\xff.txt"
			const replacement = "entry-\ufffd.txt"
			tracked := map[string]string{"ok.txt": "ok"}
			if scenario.localHasPath {
				tracked[replacement] = "old local"
			}
			dir := newRepo(t, tracked, nil, "")
			for name, contents := range map[string]string{"ok.txt": "ok", replacement: "guest created", raw: "old local"} {
				if err := os.WriteFile(filepath.Join(ws, name), []byte(contents), 0o644); err != nil {
					if name == raw && (errors.Is(err, fs.ErrInvalid) || errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.EILSEQ)) {
						t.Skipf("the guest file system rejected a non-UTF-8 file name: %v", err)
					}
					t.Fatal(err)
				}
			}
			okHash := sha256.Sum256([]byte("ok"))
			oldHash := sha256.Sum256([]byte("old local"))
			m := manifest{
				Version: manifestVersion,
				Files: map[string]manifestEntry{
					"ok.txt":    {Size: 2, SHA256: hex.EncodeToString(okHash[:]), Mode: modeFile},
					replacement: {Size: 9, SHA256: hex.EncodeToString(oldHash[:]), Mode: modeFile},
				},
				PathsValidated: scenario.clearMarkerOnLock,
			}
			writeManifest := func(m manifest) error {
				body, err := json.Marshal(m)
				if err != nil {
					return err
				}
				return os.WriteFile(ws+manifestSuffix, body, 0o644)
			}
			if err := writeManifest(m); err != nil {
				t.Fatal(err)
			}
			if scenario.clearMarkerOnLock {
				cleared := false
				srv.BeforeExec = func(req *tidev1.ExecDevSessionRequest) {
					if req.GetEnv()[guestEnvForce] != "" && !cleared {
						cleared = true
						legacy := m
						legacy.PathsValidated = false
						if err := writeManifest(legacy); err != nil {
							t.Error(err)
						}
					}
				}
			}
			ctx := context.Background()
			params := SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir}
			writes, execs := srv.Calls(writeProc), srv.Calls(execProc)
			res, err := c.Sync(ctx, params)
			if err != nil {
				t.Fatal(err)
			}
			next := guestManifest(t, ds)
			if !next.PathsValidated || next.Version != manifestVersion || next.Files["ok.txt"].Mode != modeFile {
				t.Fatalf("updated manifest = %+v", next)
			}
			if res.Deleted != 0 || res.Skipped != 1 || readGuest(t, ds, raw) != "old local" {
				t.Fatalf("sync must preserve the raw-byte guest file and skip ok.txt: %+v", res)
			}
			if scenario.localHasPath {
				if res.Uploaded != 1 || len(res.Warnings) != 0 || next.Files[replacement].Mode != modeFile ||
					readGuest(t, ds, replacement) != "old local" {
					t.Fatalf("sync must reupload the selected U+FFFD path: %+v, manifest %+v", res, next)
				}
				if err := os.Remove(filepath.Join(dir, replacement)); err != nil {
					t.Fatal(err)
				}
				res, err = c.Sync(ctx, params)
				if err != nil || res.Deleted != 1 || len(res.Warnings) != 0 {
					t.Fatalf("deletion from a validated manifest = %+v, %v", res, err)
				}
				if _, err := os.Stat(filepath.Join(ws, replacement)); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("validated U+FFFD path must be deleted: %v", err)
				}
				return
			}
			if res.Uploaded != 0 || res.BytesSent != 0 || len(res.Warnings) != 1 ||
				!strings.Contains(res.Warnings[0], strconv.Quote(replacement)) || len(next.Files) != 1 ||
				readGuest(t, ds, replacement) != "guest created" {
				t.Fatalf("sync must warn, preserve the guest file and drop the stale entry: %+v, manifest %+v", res, next)
			}
			if srv.Calls(writeProc)-writes != 1 || srv.Calls(execProc)-execs != 2 {
				t.Fatal("a manifest-only update must write the manifest and only run lock and cleanup commands")
			}
			res, err = c.Sync(ctx, params)
			if err != nil || res.Uploaded != 0 || res.Deleted != 0 || res.Skipped != 1 || len(res.Warnings) != 0 {
				t.Fatalf("unchanged sync after dropping the stale entry = %+v, %v", res, err)
			}
		})
	}
}

// caseSensitiveDir reports whether dir keeps two names that differ only in
// case as two files.
func caseSensitiveDir(t *testing.T, dir string) bool {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "case"), []byte("lower"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := os.Stat(filepath.Join(dir, "CASE"))
	return errors.Is(err, fs.ErrNotExist)
}

func testSyncCaseCollisions(t *testing.T, c *Client, srv *tidetest.Server, org string) {
	t.Helper()
	ctx := context.Background()
	collide := newRepo(t, map[string]string{"Readme.md": "1", "README.md": "2", "Src/a.go": "a", "src/b.go": "b"}, nil, "")
	for _, os := range []tidev1.GuestOs{tidev1.GuestOs_GUEST_OS_MACOS, tidev1.GuestOs_GUEST_OS_WINDOWS} {
		ds := newSession(t, c, srv, org, os)
		_, err := c.Sync(ctx, SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: collide})
		var pe *PathRuleError
		if !errors.As(err, &pe) || pe.Rule != PathRuleCaseCollision || len(pe.Paths) != 2 {
			t.Fatalf("%s: case collision error = %v", os, err)
		}
		if !strings.Contains(err.Error(), "README.md and Readme.md") {
			t.Fatalf("message %q", err.Error())
		}
	}
	// Linux keeps case: the same tree syncs.
	ds := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_LINUX)
	if _, err := c.Sync(ctx, SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: collide}); err != nil {
		t.Fatal(err)
	}

}

func TestInvalidWindowsPath(t *testing.T) {
	for p, want := range map[string]bool{
		"ok.txt": false, "dir/file.go": false, "CON": true, "con.txt": true, "lpt1.log": true, "COM9": true,
		"a<b": true, "a|b": true, "x?": true, "x*": true, `a"b`: true, "end.": true, "end ": true, "a\x01b": true,
		"console.txt": false, "aux-file": false,
	} {
		if got := invalidWindowsPath(p); got != want {
			t.Errorf("invalidWindowsPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestSyncRetryFileToDirectory(t *testing.T) {
	needPOSIX(t)
	// Exercise a nested archive parent with a leading dash and a newline.
	testSyncRetryFileToDirectory(t, tidev1.GuestOs_GUEST_OS_LINUX, "-a with space\n", func(t *testing.T, link, target string) {
		t.Helper()
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	})
}

func testSyncRetryFileToDirectory(t *testing.T, guestOS tidev1.GuestOs, former string, makeDirectoryLink func(*testing.T, string, string)) {
	t.Helper()
	for _, scenario := range []string{"first apply", "manifest write failure", "directory link", "ordinary deletion failure"} {
		t.Run(scenario, func(t *testing.T) {
			c, srv, org := newTestClient(t, "")
			ds := newSession(t, c, srv, org, guestOS)
			ws := ds.GetWorkspacePath()
			child := former + "/nested/new.txt"
			dir := newRepo(t, map[string]string{former: "old"}, nil, "")
			p := SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir}
			ctx := context.Background()
			sync := func() (*SyncResult, error) { return c.Sync(ctx, p) }
			checkCleanup := func() {
				t.Helper()
				if _, err := os.Lstat(ws + lockSuffix); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("expected lock removal, stat returned %v", err)
				}
				if matches, err := filepath.Glob(ws + ".tide-sync-*"); err != nil || len(matches) != 0 {
					t.Fatalf("temporary files left: %v, glob error: %v", matches, err)
				}
			}
			if _, err := sync(); err != nil {
				t.Fatal(err)
			}
			old := guestManifest(t, ds)
			git(t, dir, "rm", "-q", "--", former)
			if scenario == "ordinary deletion failure" {
				child = "unrelated.txt"
			}
			writeChild := filepath.Join(dir, child)
			if err := os.MkdirAll(filepath.Dir(writeChild), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(writeChild, []byte("new"), 0o644); err != nil {
				t.Fatal(err)
			}
			git(t, dir, "add", "--", child)

			var guestOnly, linkTarget string
			switch scenario {
			case "manifest write failure":
				srv.BeforeExec = func(req *tidev1.ExecDevSessionRequest) {
					if req.GetEnv()[guestEnvWorkspace] != "" && req.GetEnv()[guestEnvLock] == "" {
						srv.Fail(writeProc, connect.NewError(connect.CodePermissionDenied, errors.New("manifest write denied")))
					}
				}
				_, err := sync()
				srv.BeforeExec = nil
				if connect.CodeOf(err) != connect.CodePermissionDenied {
					t.Fatalf("manifest write error = %v", err)
				}
				if got := readGuest(t, ds, child); got != "new" {
					t.Fatalf("file after apply = %q", got)
				}
				if m := guestManifest(t, ds); m.Version != old.Version || !maps.Equal(m.Files, old.Files) {
					t.Fatalf("manifest after failed write = %+v, want %+v", m, old)
				}
				checkCleanup()
				guestOnly = former + "/guest.txt"
				if err := os.WriteFile(filepath.Join(ws, child), []byte("guest edit"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "directory link", "ordinary deletion failure":
				if err := os.Remove(filepath.Join(ws, former)); err != nil {
					t.Fatal(err)
				}
				if scenario == "directory link" {
					linkTarget = t.TempDir()
					if err := os.WriteFile(filepath.Join(linkTarget, "keep.txt"), []byte("keep"), 0o644); err != nil {
						t.Fatal(err)
					}
					makeDirectoryLink(t, filepath.Join(ws, former), linkTarget)
				} else {
					if err := os.Mkdir(filepath.Join(ws, former), 0o755); err != nil {
						t.Fatal(err)
					}
					guestOnly = former + "/guest.txt"
				}
			}
			if guestOnly != "" {
				if err := os.WriteFile(filepath.Join(ws, guestOnly), []byte("keep"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			res, err := sync()
			checkCleanup()
			if guestOnly != "" && readGuest(t, ds, guestOnly) != "keep" {
				t.Fatal("sync changed a guest-only file")
			}
			if scenario == "ordinary deletion failure" {
				if connect.CodeOf(err) != connect.CodeInternal || !strings.Contains(err.Error(), "the guest apply command exited") {
					t.Fatalf("ordinary deletion should fail in guest apply, got %v", err)
				}
				if _, err := os.Lstat(filepath.Join(ws, child)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("archive extracted after a failed deletion, stat returned %v", err)
				}
				if m := guestManifest(t, ds); m.Version != old.Version || !maps.Equal(m.Files, old.Files) {
					t.Fatalf("manifest after failed deletion = %+v, want %+v", m, old)
				}
				return
			}
			if err != nil || res.Uploaded != 1 || res.Deleted != 1 {
				t.Fatalf("file-to-directory sync = %+v, %v", res, err)
			}
			if got := readGuest(t, ds, child); got != "new" {
				t.Fatalf("file after sync = %q", got)
			}
			if linkTarget != "" {
				if info, err := os.Lstat(filepath.Join(ws, former)); err != nil || !info.IsDir() {
					t.Fatalf("sync did not replace the symlink with a directory: %v, %v", info, err)
				}
				if data, err := os.ReadFile(filepath.Join(linkTarget, "keep.txt")); err != nil || string(data) != "keep" {
					t.Fatalf("symlink target changed: %q, %v", data, err)
				}
				if _, err := os.Lstat(filepath.Join(linkTarget, "nested")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("sync wrote through the symlink, stat returned %v", err)
				}
			}
			m := guestManifest(t, ds)
			if _, ok := m.Files[child]; !ok || len(m.Files) != 1 || m.Version != old.Version {
				t.Fatalf("manifest after sync = %+v", m)
			}
			writes, execs := srv.Calls(writeProc), srv.Calls(execProc)
			res, err = sync()
			if err != nil || res.Uploaded != 0 || res.Deleted != 0 || res.Skipped != 1 {
				t.Fatalf("unchanged sync = %+v, %v", res, err)
			}
			if srv.Calls(writeProc) != writes || srv.Calls(execProc)-execs != 2 {
				t.Fatal("unchanged sync must only run lock and cleanup commands without writing files")
			}
		})
	}
}

// TestSyncReplacesDirectoryWithFile checks that a synced directory that
// became a file locally is replaced in the guest: the deletions leave empty
// directories, which the apply command removes before the extraction, but
// it keeps a directory that holds files that the guest created.
func TestSyncReplacesDirectoryWithFile(t *testing.T) {
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	ctx := context.Background()
	for _, guestFile := range []bool{false, true} {
		ds := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_LINUX)
		dir := newRepo(t, map[string]string{"a/b/old": "old", "a/c": "c", "keep.txt": "k"}, nil, "")
		if _, err := c.Sync(ctx, SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir}); err != nil {
			t.Fatal(err)
		}
		if guestFile {
			if err := os.WriteFile(filepath.Join(ds.GetWorkspacePath(), "a/b/guest.txt"), []byte("guest"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		git(t, dir, "rm", "-q", "-r", "a")
		if err := os.WriteFile(filepath.Join(dir, "a"), []byte("now a file"), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, dir, "add", "a")
		res, err := c.Sync(ctx, SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir})
		if guestFile {
			if err == nil {
				t.Fatal("the sync removed a directory that holds a guest file")
			}
			if readGuest(t, ds, "a/b/guest.txt") != "guest" {
				t.Fatal("the guest file is gone")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if res.Uploaded != 1 || res.Deleted != 2 || readGuest(t, ds, "a") != "now a file" {
			t.Fatalf("sync = %+v", res)
		}
		if matches, _ := filepath.Glob(ds.GetWorkspacePath() + ".tide-sync-*"); len(matches) != 0 {
			t.Fatalf("temporary files left: %v", matches)
		}
	}
}

func TestSyncUnstagedDirectoryToFile(t *testing.T) {
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	ctx := context.Background()
	for _, scenario := range []struct {
		name      string
		guestFile bool
	}{
		{name: "empty after deletion"},
		{name: "guest file blocks replacement", guestFile: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ds := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_LINUX)
			dir := newRepo(t, map[string]string{"a/b/old": "old", "a/c": "c", "keep.txt": "k"}, nil, "")
			params := SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir}
			if _, err := c.Sync(ctx, params); err != nil {
				t.Fatal(err)
			}
			before := guestManifest(t, ds)
			if scenario.guestFile {
				if err := os.WriteFile(filepath.Join(ds.GetWorkspacePath(), "a/b/guest.txt"), []byte("guest"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			replacement := filepath.Join(dir, "a")
			if err := os.RemoveAll(replacement); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(replacement, []byte("now a file"), 0o644); err != nil {
				t.Fatal(err)
			}
			listed := strings.Split(strings.TrimSpace(git(t, dir, "ls-files", "--cached", "--others", "--exclude-standard")), "\n")
			for _, p := range []string{"a", "a/b/old", "a/c"} {
				if !slices.Contains(listed, p) {
					t.Fatalf("git omitted %q from %v", p, listed)
				}
			}

			res, err := c.Sync(ctx, params)
			git(t, dir, "diff", "--cached", "--quiet")
			after := guestManifest(t, ds)
			if scenario.guestFile {
				if connect.CodeOf(err) != connect.CodeInternal || !strings.Contains(err.Error(), "guest apply command") {
					t.Fatalf("sync = %+v, %v, want guest apply error", res, err)
				}
				if got := readGuest(t, ds, "a/b/guest.txt"); got != "guest" {
					t.Fatalf("guest file content = %q", got)
				}
				if after.Version != before.Version || after.PathsValidated != before.PathsValidated || !maps.Equal(after.Files, before.Files) {
					t.Fatalf("manifest after failed replacement = %+v, want %+v", after, before)
				}
				return
			}
			if err != nil || res.Uploaded != 1 || res.Deleted != 2 {
				t.Fatalf("sync = %+v, %v", res, err)
			}
			if readGuest(t, ds, "a") != "now a file" || readGuest(t, ds, "keep.txt") != "k" {
				t.Fatal("guest has unexpected file contents")
			}
			if len(after.Files) != 2 || after.Files["a"].Mode != modeFile || after.Files["keep.txt"] != before.Files["keep.txt"] {
				t.Fatalf("manifest after replacement = %+v", after)
			}
		})
	}
}

func TestReadLocalFileLookupError(t *testing.T) {
	needPOSIX(t)
	_, ok, err := readLocalFile(t.TempDir(), strings.Repeat("a", 4096), "")
	if ok || !errors.Is(err, syscall.ENAMETOOLONG) {
		t.Fatalf("readLocalFile = present %t, error %v, want ENAMETOOLONG", ok, err)
	}
}

func TestPruneDirs(t *testing.T) {
	changed := []localFile{{rel: "a"}, {rel: "x/y"}}
	got, aliases := pruneDirs([]string{"a/b/c/old", "a/d", "x/y/z", "other/dir/f", "a"}, changed, false)
	want := []string{"a/b/c", "a/b", "x/y", "a"}
	if !slices.Equal(got, want) || len(aliases) != 0 {
		t.Fatalf("pruneDirs = %q, %q; want %q and no aliases", got, aliases, want)
	}
}

func TestPruneDirsCaseOnlyReplacement(t *testing.T) {
	deleted := []string{"Assets/nested/old.txt", "Assets/nested/another.txt", "Exact/sub/old.txt"}
	changed := []localFile{{rel: "assets"}, {rel: "Exact"}}
	wantExact := []string{"Exact/sub", "Exact"}
	for _, matchAliases := range []bool{false, true} {
		var wantAliases []string
		if matchAliases {
			wantAliases = []string{"Assets", "assets", "Assets/nested", "Assets", "assets", "Assets"}
		}
		exact, aliases := pruneDirs(deleted, changed, matchAliases)
		if !slices.Equal(exact, wantExact) || !slices.Equal(aliases, wantAliases) {
			t.Fatalf("pruneDirs(matchAliases=%t) = %q, %q; want %q, %q", matchAliases, exact, aliases, wantExact, wantAliases)
		}
	}
}

func TestPruneDirsApplyAliasRecords(t *testing.T) {
	needPOSIX(t)
	dir := t.TempDir()
	ws := filepath.Join(dir, "work")
	old := "-Assets\n'"
	nested := old + "/nested\n'"
	if err := os.MkdirAll(filepath.Join(ws, nested), 0o755); err != nil {
		t.Fatal(err)
	}
	// A second spelling with ./ refers to the same directory on any volume.
	incoming := "./" + old
	list := filepath.Join(dir, "alias-prune-list")
	if err := os.WriteFile(list, pathList([]string{old, incoming, nested, old, incoming, old}, false), 0o644); err != nil {
		t.Fatal(err)
	}
	args, err := guestCommand(tidev1.GuestOs_GUEST_OS_MACOS, opApply)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(args[2], "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "xargs" && slices.Contains(fields, "-x") {
			t.Fatalf("xargs -x splits NUL-separated alias records on macOS: %s", line)
		}
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), guestEnvWorkspace + "=" + ws, guestEnvAliasPrunes + "=" + list}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("prune alias directories: %v: %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(ws, old)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old directory was not removed: %v", err)
	}
}

// TestSyncPlansAgainUnderTheLock starts with a matching manifest. Another
// sync replaces a.txt and the manifest before this sync takes the lock.
func TestSyncPlansAgainUnderTheLock(t *testing.T) {
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	ds := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_LINUX)
	ws := ds.GetWorkspacePath()
	dir := newRepo(t, map[string]string{"a.txt": "v1"}, nil, "")
	ctx := context.Background()
	if _, err := c.Sync(ctx, SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir}); err != nil {
		t.Fatal(err)
	}
	want := guestManifest(t, ds)
	other := false
	srv.BeforeExec = func(req *tidev1.ExecDevSessionRequest) {
		if other || req.GetEnv()[guestEnvLock] == "" || req.GetEnv()[guestEnvForce] == "" {
			return
		}
		// The other sync ends just before this sync takes the lock.
		other = true
		if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("v2"), 0o644); err != nil {
			t.Error(err)
		}
		m := guestManifest(t, ds)
		sum := sha256.Sum256([]byte("v2"))
		m.Files["a.txt"] = manifestEntry{Size: 2, SHA256: hex.EncodeToString(sum[:]), Mode: modeFile}
		b, _ := json.Marshal(m)
		if err := os.WriteFile(ws+manifestSuffix, b, 0o644); err != nil {
			t.Error(err)
		}
	}
	res, err := c.Sync(ctx, SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if !other {
		t.Fatal("the sync took no lock")
	}
	if res.Uploaded != 1 || readGuest(t, ds, "a.txt") != "v1" {
		t.Fatalf("sync = %+v, a.txt = %q", res, readGuest(t, ds, "a.txt"))
	}
	if m := guestManifest(t, ds); !maps.Equal(m.Files, want.Files) {
		t.Fatalf("manifest = %+v, want %+v", m, want)
	}
	if _, err := os.Stat(ws + lockSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the sync kept its lock")
	}
}

func TestSyncUnchangedTakesLock(t *testing.T) {
	needPOSIX(t)
	const warning = "the guest could not release the sync lock; the next sync may need force"
	for _, tc := range []struct {
		name        string
		existing    bool
		force       bool
		failCleanup bool
	}{
		{name: "held lock", existing: true},
		{name: "force stale lock", existing: true, force: true},
		{name: "cleanup failure", failCleanup: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c, srv, org := newTestClient(t, "")
			ds := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_LINUX)
			dir := newRepo(t, map[string]string{"a.txt": "v1"}, nil, "")
			params := SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir}
			if _, err := c.Sync(ctx, params); err != nil {
				t.Fatal(err)
			}
			ws := ds.GetWorkspacePath()
			lock := ws + lockSuffix
			marker := filepath.Join(lock, "existing")
			if tc.existing {
				if err := os.Mkdir(lock, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(marker, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			readsBefore := len(srv.ReadRequests())
			execsBefore := srv.Calls(execProc)
			writesBefore := srv.Calls(writeProc)
			srv.BeforeExec = func(req *tidev1.ExecDevSessionRequest) {
				env := req.GetEnv()
				switch {
				case env[guestEnvLock] == lock && env[guestEnvForce] != "":
					if n := len(srv.ReadRequests()) - readsBefore; n != 1 {
						t.Errorf("manifest reads before lock = %d, want 1", n)
					}
					if (env[guestEnvForce] == "1") != tc.force {
						t.Errorf("force lock value = %q, force = %t", env[guestEnvForce], tc.force)
					}
					if tc.failCleanup {
						srv.Fail(execProc, connect.NewError(connect.CodePermissionDenied, errors.New("cleanup denied")))
					}
				case env[guestEnvLock] == lock && env[guestEnvWorkspace] == "":
					if reads := srv.ReadRequests()[readsBefore:]; len(reads) != 2 || reads[1].GetPath() != ws+manifestSuffix {
						t.Errorf("expected a second manifest read before cleanup, got %v", reads)
					}
					if _, err := os.Stat(lock); err != nil {
						t.Errorf("lock before cleanup: %v", err)
					}
					if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("stale lock marker before cleanup: %v", err)
					}
				default:
					t.Error("unchanged sync ran an unexpected guest command")
				}
			}
			var phases []TransferPhase
			params.Force = tc.force
			params.Progress = func(p TransferProgress) { phases = append(phases, p.Phase) }
			res, err := c.Sync(ctx, params)
			if srv.Calls(writeProc) != writesBefore {
				t.Fatal("unchanged sync wrote to the guest")
			}
			if tc.existing && !tc.force {
				if res != nil || !errors.Is(err, ErrSyncLocked) || connect.CodeOf(err) != connect.CodeFailedPrecondition {
					t.Fatalf("locked sync = %+v, %v", res, err)
				}
				if _, err := os.Stat(marker); err != nil {
					t.Fatalf("existing lock marker: %v", err)
				}
				return
			}
			if err != nil || res.Uploaded != 0 || res.Deleted != 0 || res.Skipped != 1 || res.BytesSent != 0 {
				t.Fatalf("unchanged sync = %+v, %v", res, err)
			}
			if n := srv.Calls(execProc) - execsBefore; n != 2 {
				t.Fatalf("guest commands = %d, want lock and cleanup", n)
			}
			if !slices.Equal(phases, []TransferPhase{TransferPreparing, TransferDone}) {
				t.Fatalf("unchanged sync phases = %v", phases)
			}
			_, statErr := os.Stat(lock)
			if tc.failCleanup {
				if !slices.Equal(res.Warnings, []string{warning}) || statErr != nil {
					t.Fatalf("cleanup failure warnings = %v, lock stat = %v", res.Warnings, statErr)
				}
			} else if len(res.Warnings) != 0 || !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("cleanup warnings = %v, lock stat = %v", res.Warnings, statErr)
			}
		})
	}
}

func TestPOSIXSyncCleanup(t *testing.T) {
	needPOSIX(t)
	for _, scenario := range []struct {
		name        string
		archiveDir  bool
		lockDotPath bool
	}{
		{name: "archive removal fails", archiveDir: true},
		{name: "missing and empty paths"},
		{name: "lock removal fails", lockDotPath: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			dir := t.TempDir()
			archive := filepath.Join(dir, "archive.tar")
			lock := filepath.Join(dir, "lock")
			if err := os.Mkdir(lock, 0o755); err != nil {
				t.Fatal(err)
			}
			if scenario.archiveDir {
				if err := os.Mkdir(archive, 0o755); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(archive, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			lockArg := lock
			if scenario.lockDotPath {
				lockArg += "/."
			}
			args, err := guestCommand(tidev1.GuestOs_GUEST_OS_LINUX, opCleanup)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(args[0], args[1:]...)
			cmd.Env = []string{
				"PATH=" + os.Getenv("PATH"),
				guestEnvArchive + "=" + archive,
				guestEnvLock + "=" + lockArg,
			}
			for _, key := range []string{guestEnvDeletes, guestEnvReplaceFiles, guestEnvPrunes, guestEnvAliasPrunes} {
				path := filepath.Join(dir, key)
				if scenario.archiveDir {
					if err := os.WriteFile(path, nil, 0o644); err != nil {
						t.Fatal(err)
					}
					cmd.Env = append(cmd.Env, key+"="+path)
				} else if key == guestEnvDeletes {
					cmd.Env = append(cmd.Env, key+"="+path)
				} else if key == guestEnvReplaceFiles {
					cmd.Env = append(cmd.Env, key+"=")
				}
			}
			out, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if scenario.archiveDir || scenario.lockDotPath {
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
					t.Errorf("cleanup should exit 1, got %v: %s", err, out)
				}
			} else if err != nil {
				t.Errorf("cleanup should succeed, got %v: %s", err, out)
			}
			var leftovers []string
			if scenario.archiveDir {
				leftovers = append(leftovers, filepath.Base(archive))
			}
			if scenario.lockDotPath {
				leftovers = append(leftovers, filepath.Base(lock))
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			if !slices.Equal(names, leftovers) {
				t.Errorf("remaining paths = %v, want %v", names, leftovers)
			}
		})
	}
}

func TestSyncCleanupWarnings(t *testing.T) {
	needPOSIX(t)
	for _, scenario := range []string{"committed sync", "manifest write fails"} {
		t.Run(scenario, func(t *testing.T) {
			c, srv, org := newTestClient(t, "")
			ds := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_LINUX)
			dir := newRepo(t, map[string]string{"a.txt": "a"}, nil, "")
			var logs bytes.Buffer
			c.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			srv.BeforeExec = func(req *tidev1.ExecDevSessionRequest) {
				env := req.GetEnv()
				if env[guestEnvWorkspace] != "" && env[guestEnvLock] == "" && scenario == "manifest write fails" {
					srv.Fail(writeProc, connect.NewError(connect.CodePermissionDenied, errors.New("manifest write denied")))
				}
				if archive := env[guestEnvArchive]; archive != "" && env[guestEnvLock] != "" && env[guestEnvForce] == "" && env[guestEnvWorkspace] == "" {
					// rm -f fails on a directory even when the test runs as root.
					if err := os.Remove(archive); err != nil {
						t.Error(err)
						return
					}
					if err := os.Mkdir(archive, 0o755); err != nil {
						t.Error(err)
					}
				}
			}
			res, err := c.Sync(context.Background(), SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir})
			ws := ds.GetWorkspacePath()
			if scenario == "manifest write fails" {
				var connectErr *connect.Error
				if res != nil || !errors.As(err, &connectErr) || connectErr.Code() != connect.CodePermissionDenied || connectErr.Message() != "manifest write denied" {
					t.Fatalf("sync = %+v, %v, want manifest write error", res, err)
				}
				if _, err := os.Stat(ws + manifestSuffix); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("manifest after failed write: %v", err)
				}
				if !strings.Contains(logs.String(), "tide sync clean-up failed") {
					t.Error("missing deferred cleanup debug message")
				}
			} else {
				if err != nil || res == nil || res.Uploaded != 1 {
					t.Fatalf("sync = %+v, %v", res, err)
				}
				const warning = "the guest could not remove the sync archive or release the sync lock; the next sync may need force"
				if !slices.Contains(res.Warnings, warning) {
					t.Errorf("cleanup warning missing from %v", res.Warnings)
				}
				if m := guestManifest(t, ds); m.Version != manifestVersion || len(m.Files) != 1 || m.Files["a.txt"].Size != 1 {
					t.Errorf("committed manifest = %+v", m)
				}
			}
			if got := readGuest(t, ds, "a.txt"); got != "a" {
				t.Errorf("applied file = %q", got)
			}
			if _, err := os.Stat(ws + lockSuffix); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("sync lock should be removed, got %v", err)
			}
		})
	}
}

func TestSyncLockAndRestartAfterStoppedSync(t *testing.T) {
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	ds := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_LINUX)
	dir := newRepo(t, map[string]string{"a.txt": "a", "b.txt": "b"}, nil, "")
	ctx := context.Background()

	// A lock that a stopped sync left.
	lock := ds.GetWorkspacePath() + lockSuffix
	if err := os.Mkdir(lock, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := c.Sync(ctx, SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir})
	if !errors.Is(err, ErrSyncLocked) || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("locked sync = %v", err)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatal("a locked sync removed the other sync's lock")
	}
	if srv.Calls(writeProc) != 0 {
		t.Fatal("a locked sync uploaded")
	}

	// A sync that stops during the upload leaves the old (missing)
	// manifest and releases its lock; the next sync sends everything.
	srv.Fail(writeProc, connect.NewError(connect.CodePermissionDenied, errors.New("stopped")))
	_, err = c.Sync(ctx, SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir, Force: true})
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("stopped sync = %v", err)
	}
	if _, err := os.Stat(ds.GetWorkspacePath() + manifestSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a stopped sync wrote the manifest")
	}
	if _, err := os.Stat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a failed sync kept its lock")
	}
	res, err := c.Sync(ctx, SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir})
	if err != nil || res.Uploaded != 2 {
		t.Fatalf("restart = %+v %v", res, err)
	}

	// An unreadable manifest counts as empty.
	if err := os.WriteFile(ds.GetWorkspacePath()+manifestSuffix, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = c.Sync(ctx, SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir})
	if err != nil || res.Uploaded != 2 {
		t.Fatalf("sync after a broken manifest = %+v %v", res, err)
	}

	// A session that is not running fails before any guest command.
	srv.SetSessionState(ds.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_STARTING)
	if _, err := c.Sync(ctx, SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir}); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("sync of a starting session = %v", err)
	}
}

func TestSyncApplyStreamLoss(t *testing.T) {
	needPOSIX(t)
	for _, tc := range []struct {
		name    string
		unknown bool
	}{
		{name: "terminal"},
		{name: "unknown", unknown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, stop := context.WithTimeout(t.Context(), 15*time.Second)
			defer stop()
			c, srv, org := newTestClient(t, "")
			ds := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_LINUX)
			params := func(content string) SyncParams {
				return SyncParams{
					OrganizationID: org, DevSessionID: ds.GetDevSessionId(),
					LocalDir: newRepo(t, map[string]string{"a.txt": content}, nil, ""),
				}
			}
			initial, first, second := params("initial"), params("first"), params("second")
			if err := os.WriteFile(filepath.Join(initial.LocalDir, "obsolete.txt"), []byte("obsolete"), 0o644); err != nil {
				t.Fatal(err)
			}
			lock := ds.GetWorkspacePath() + lockSuffix
			var manifestAtCleanup atomic.Pointer[manifest]
			srv.BeforeExec = func(req *tidev1.ExecDevSessionRequest) {
				env := req.GetEnv()
				if env[guestEnvLock] != lock || env[guestEnvArchive] == "" {
					return
				}
				body, err := os.ReadFile(ds.GetWorkspacePath() + manifestSuffix)
				if err != nil {
					t.Error(err)
					return
				}
				var published manifest
				if err := json.Unmarshal(body, &published); err != nil {
					t.Error(err)
					return
				}
				manifestAtCleanup.Store(&published)
			}
			if _, err := c.Sync(ctx, initial); err != nil {
				t.Fatal(err)
			}
			old := guestManifest(t, ds)
			requireOldManifest := func() {
				t.Helper()
				if got := guestManifest(t, ds); !maps.Equal(got.Files, old.Files) {
					t.Fatalf("manifest entries changed before a successful sync: %+v", got.Files)
				}
			}

			// Run the actual apply script and hold tar just before extraction.
			// Its process remains alive when the client cancels the stream.
			tarBin, err := exec.LookPath("tar")
			if err != nil {
				t.Fatal(err)
			}
			gate := t.TempDir()
			script := `#!/bin/sh
set -eu
: > "$TIDE_TEST_GATE/started"
while [ ! -f "$TIDE_TEST_GATE/release" ]; do sleep 0.02; done
exec "$TIDE_TEST_REAL_TAR" "$@"
`
			if err := os.WriteFile(filepath.Join(gate, "tar"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TIDE_TEST_GATE", gate)
			t.Setenv("TIDE_TEST_REAL_TAR", tarBin)
			t.Setenv("PATH", gate+string(os.PathListSeparator)+os.Getenv("PATH"))

			syncErr := make(chan error, 1)
			applyCtx, cancelApply := context.WithCancel(ctx)
			defer cancelApply()
			go func() {
				_, err := c.Sync(applyCtx, first)
				syncErr <- err
			}()
			waitFor := func(description string, ready func() bool) {
				t.Helper()
				ticker := time.NewTicker(5 * time.Millisecond)
				defer ticker.Stop()
				for !ready() {
					select {
					case err := <-syncErr:
						t.Fatalf("sync returned before %s: %v", description, err)
					case <-ctx.Done():
						t.Fatalf("waiting for %s: %v", description, ctx.Err())
					case <-ticker.C:
					}
				}
			}
			waitFor("apply to reach tar", func() bool {
				_, err := os.Stat(filepath.Join(gate, "started"))
				return err == nil
			})
			requests := srv.ExecRequests()
			apply := requests[len(requests)-1]
			archive, deletes := apply.GetEnv()[guestEnvArchive], apply.GetEnv()[guestEnvDeletes]
			if archive == "" || deletes == "" {
				t.Fatal("the apply request needs an archive and deletion list")
			}
			retainedPaths := []string{lock, archive, deletes}
			attachProc := tidev1connect.DevSessionServiceAttachDevSessionExecProcedure
			attachments := srv.Calls(attachProc)
			if tc.unknown {
				srv.Fail(attachProc, connect.NewError(connect.CodeNotFound, errors.New("exec unknown")))
			}
			cancelApply()
			requireCanceled := func() {
				t.Helper()
				select {
				case err := <-syncErr:
					var streamErr *ExecStreamError
					if !errors.As(err, &streamErr) || !errors.Is(err, context.Canceled) || streamErr.ExecID != apply.GetExecId() {
						t.Fatalf("sync returned %v, want original cancellation for apply %s", err, apply.GetExecId())
					}
				case <-ctx.Done():
					t.Fatalf("canceled sync did not return: %v", ctx.Err())
				}
			}
			if tc.unknown {
				requireCanceled()
			} else {
				waitFor("SDK to attach to apply", func() bool { return srv.Calls(attachProc) > attachments })
			}

			for _, path := range retainedPaths {
				if _, err := os.Stat(path); err != nil {
					t.Fatalf("apply still needs %s: %v", path, err)
				}
			}
			writes := srv.Calls(writeProc)
			if _, err := c.Sync(ctx, second); !errors.Is(err, ErrSyncLocked) {
				t.Fatalf("concurrent sync returned %v, want ErrSyncLocked", err)
			}
			if got := srv.Calls(writeProc); got != writes {
				t.Errorf("concurrent sync issued %d write calls", got-writes)
			}
			requireOldManifest()
			if err := os.WriteFile(filepath.Join(gate, "release"), nil, 0o644); err != nil {
				t.Fatal(err)
			}

			if tc.unknown {
				// The test observes apply directly so it can verify that a
				// command with an unknown outcome could still write.
				result, err := c.AttachExec(ctx, AttachParams{
					OrganizationID: org, DevSessionID: ds.GetDevSessionId(), ExecID: apply.GetExecId(),
				})
				if err != nil || result.Kind != ExecResultExited {
					t.Fatalf("observe original apply = %+v, %v", result, err)
				}
			} else {
				requireCanceled()
			}
			if got := readGuest(t, ds, "a.txt"); got != "first" {
				t.Fatalf("original apply wrote %q, want first", got)
			}
			requireOldManifest()
			applyCount := 0
			for _, req := range srv.ExecRequests() {
				if req.GetEnv()[guestEnvArchive] != "" && req.GetEnv()[guestEnvWorkspace] != "" {
					applyCount++
				}
			}
			if applyCount != 2 { // Initial sync and the canceled sync each applied once.
				t.Errorf("got %d apply commands, want 2", applyCount)
			}
			for _, path := range retainedPaths {
				_, err := os.Stat(path)
				if tc.unknown && err != nil || !tc.unknown && !errors.Is(err, os.ErrNotExist) {
					t.Errorf("stat %s after %s outcome: %v", path, tc.name, err)
				}
			}
			if tc.unknown {
				return
			}
			if res, err := c.Sync(ctx, second); err != nil || res.Uploaded != 1 {
				t.Fatalf("retry sync = %+v, %v", res, err)
			}
			if got := readGuest(t, ds, "a.txt"); got != "second" {
				t.Errorf("retry wrote %q, want second", got)
			}
			if _, err := os.Stat(lock); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("lock after successful retry: %v", err)
			}
			hash := sha256.Sum256([]byte("second"))
			if m := manifestAtCleanup.Load(); m == nil || m.Files["a.txt"].SHA256 != hex.EncodeToString(hash[:]) {
				t.Errorf("manifest at lock release = %+v, want second file hash", m)
			}
		})
	}
}

func TestDownloadFileAndDirectory(t *testing.T) {
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	ds := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_LINUX)
	ws := ds.GetWorkspacePath()
	mustWrite := func(p, content string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(filepath.Join(ws, "out/bin/app"), "binary", 0o755)
	mustWrite(filepath.Join(ws, "out/report.txt"), "report", 0o644)
	if err := os.Symlink("bin/app", filepath.Join(ws, "out/current")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	local := t.TempDir()

	res, err := c.Download(ctx, DownloadParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), GuestPath: "out", LocalPath: filepath.Join(local, "result")})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Directory || res.Files != 3 || res.Bytes != int64(len("binary")+len("report")) {
		t.Fatalf("directory download = %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(local, "result/bin/app")); string(b) != "binary" {
		t.Fatal("downloaded content differs")
	}
	if target, err := os.Readlink(filepath.Join(local, "result/current")); err != nil || target != "bin/app" {
		t.Fatalf("downloaded link %q %v", target, err)
	}
	if matches, _ := filepath.Glob(ws + ".tide-download-*"); len(matches) != 0 {
		t.Fatalf("guest archive left: %v", matches)
	}
	if matches, _ := filepath.Glob(filepath.Join(local, ".tide-download-*")); len(matches) != 0 {
		t.Fatalf("local temporary files left: %v", matches)
	}

	// A second download replaces the destination.
	mustWrite(filepath.Join(ws, "out/report.txt"), "report 2", 0o644)
	if _, err := c.Download(ctx, DownloadParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), GuestPath: ws + "/out", LocalPath: filepath.Join(local, "result")}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(local, "result/report.txt")); string(b) != "report 2" {
		t.Fatal("replaced content differs")
	}

	res, err = c.Download(ctx, DownloadParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), GuestPath: "out/report.txt", LocalPath: filepath.Join(local, "nested/report.txt")})
	if err != nil || res.Directory || res.Files != 1 {
		t.Fatalf("file download = %+v %v", res, err)
	}
	if b, _ := os.ReadFile(filepath.Join(local, "nested/report.txt")); string(b) != "report 2" {
		t.Fatal("file content differs")
	}

	t.Run("failed installation", func(t *testing.T) {
		parent := t.TempDir()
		dest := filepath.Join(parent, "result")
		mustWrite(dest, "old", 0o644)
		var staged string
		var phases []TransferPhase
		res, err := c.Download(t.Context(), DownloadParams{
			OrganizationID: org, DevSessionID: ds.GetDevSessionId(), GuestPath: "out/report.txt", LocalPath: dest,
			Progress: func(p TransferProgress) {
				phases = append(phases, p.Phase)
				if p.Phase == TransferDownloading && staged == "" {
					// Remove the staged file after it receives data and before installation.
					dirs, err := filepath.Glob(filepath.Join(parent, ".tide-download-*"))
					if err != nil || len(dirs) != 1 {
						t.Fatalf("staging directories = %v, %v", dirs, err)
					}
					staged = filepath.Join(dirs[0], "result")
					if err := os.Remove(staged); err != nil {
						t.Fatal(err)
					}
				}
			},
		})
		var renameErr *os.LinkError
		if res != nil || !errors.Is(err, os.ErrNotExist) || !errors.As(err, &renameErr) ||
			renameErr.Old != staged || renameErr.New != dest || slices.Contains(phases, TransferDone) {
			t.Fatalf("failed installation = %+v, %v, phases %v", res, err, phases)
		}
		if contents, err := os.ReadFile(dest); err != nil || string(contents) != "old" {
			t.Fatalf("restored destination = %q, %v", contents, err)
		}
	})

	_, err = c.Download(ctx, DownloadParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), GuestPath: "missing", LocalPath: filepath.Join(local, "x")})
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("missing guest path = %v", err)
	}
	if _, err := os.Stat(filepath.Join(local, "x")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a failed download created the destination")
	}

	// A file over the limit is rejected before it is read.
	big := filepath.Join(ws, "big.bin")
	f, _ := os.Create(big)
	_ = f.Truncate(DownloadFileLimit + 1)
	f.Close()
	reads := srv.Calls("/delino.tide.v1.DevSessionService/ReadDevSessionFile")
	_, err = c.Download(ctx, DownloadParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), GuestPath: "big.bin", LocalPath: filepath.Join(local, "big")})
	var le *LimitError
	if !errors.As(err, &le) || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("download over the limit = %v", err)
	}
	if srv.Calls("/delino.tide.v1.DevSessionService/ReadDevSessionFile") != reads {
		t.Fatal("a download over the limit read the file")
	}
}

func TestDownloadHardLinkFallback(t *testing.T) {
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	ds := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_LINUX)
	guest := filepath.Join(ds.GetWorkspacePath(), "out")
	if err := os.Mkdir(guest, 0o755); err != nil {
		t.Fatal(err)
	}
	const content = "linked content"
	const size = int64(len(content))
	source := filepath.Join(guest, "source")
	if err := os.WriteFile(source, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	names := []string{"source", "link1", "link2"}
	for _, name := range names[1:] {
		if err := os.Link(source, filepath.Join(guest, name)); err != nil {
			t.Skipf("guest fixture needs hard links: %v", err)
		}
	}
	unavailable := func(*os.Root, string, string) error { return errors.ErrUnsupported }
	for _, tc := range []struct {
		name       string
		extractor  archiveExtractor
		wantBytes  int64
		wantLinked bool
		wantTotal  int64
	}{
		{name: "hard link", wantBytes: size, wantLinked: true},
		{name: "fallback at limit", extractor: archiveExtractor{fileLimit: size, totalLimit: 3 * size, link: unavailable}, wantBytes: 3 * size},
		{name: "fallback over limit", extractor: archiveExtractor{fileLimit: size, totalLimit: 2 * size, link: unavailable}, wantTotal: 3 * size},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			dest := filepath.Join(parent, "result")
			if err := os.Mkdir(dest, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dest, "old"), []byte("previous"), 0o644); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			params := DownloadParams{
				OrganizationID: org, DevSessionID: ds.GetDevSessionId(), GuestPath: "out", LocalPath: dest,
			}
			download := c.Download
			if tc.extractor.link != nil {
				download = func(ctx context.Context, p DownloadParams) (*DownloadResult, error) {
					return c.download(ctx, p, tc.extractor)
				}
			}
			res, err := download(ctx, params)
			if entries, err := os.ReadDir(parent); err != nil || len(entries) != 1 || entries[0].Name() != "result" {
				t.Fatalf("destination parent entries = %v, %v", entries, err)
			}
			if tc.wantTotal > 0 {
				var limit *LimitError
				if res != nil || connect.CodeOf(err) != connect.CodeFailedPrecondition || !errors.As(err, &limit) || limit.Total != tc.wantTotal {
					t.Fatalf("over-limit download = %+v, %v", res, err)
				}
				if entries, err := os.ReadDir(dest); err != nil || len(entries) != 1 || entries[0].Name() != "old" {
					t.Fatalf("original destination entries = %v, %v", entries, err)
				}
				if b, err := os.ReadFile(filepath.Join(dest, "old")); err != nil || string(b) != "previous" {
					t.Fatalf("original destination = %q, %v", b, err)
				}
				return
			}
			if err != nil || res == nil || !res.Directory || res.Files != len(names) || res.Bytes != tc.wantBytes {
				t.Fatalf("directory download = %+v, %v", res, err)
			}
			infos := make([]fs.FileInfo, 0, len(names))
			for _, name := range names {
				p := filepath.Join(dest, name)
				if b, err := os.ReadFile(p); err != nil || string(b) != content {
					t.Fatalf("%s content = %q, %v", name, b, err)
				}
				info, err := os.Stat(p)
				if err != nil {
					t.Fatal(err)
				}
				for _, previous := range infos {
					if os.SameFile(previous, info) != tc.wantLinked {
						t.Errorf("same file (%s, %s) should be %t", previous.Name(), info.Name(), tc.wantLinked)
					}
				}
				infos = append(infos, info)
			}
		})
	}
}

func TestDownloadBackupCleanup(t *testing.T) {
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	ds := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_LINUX)
	guest := filepath.Join(ds.GetWorkspacePath(), "out")
	if err := os.Mkdir(guest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(guest, "new.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A FIFO gives the archive an unsupported entry and an extraction warning.
	if out, err := exec.Command("mkfifo", filepath.Join(guest, "pipe")).CombinedOutput(); err != nil {
		t.Fatalf("mkfifo: %v: %s", err, out)
	}
	const extractionWarning = "pipe: skipped an archive entry of an unsupported type"

	for _, tc := range []struct {
		name string
		mode os.FileMode
	}{
		{name: "cleanup succeeds", mode: 0o755},
		{name: "cleanup denied", mode: 0o555},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanupDenied := tc.mode == 0o555
			if cleanupDenied && os.Geteuid() == 0 {
				t.Skip("the cleanup failure requires an unprivileged user")
			}
			parent := t.TempDir()
			dest := filepath.Join(parent, "result")
			const oldDir = "restricted"
			if err := os.MkdirAll(filepath.Join(dest, oldDir), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dest, oldDir, "old.txt"), []byte("old"), 0o644); err != nil {
				t.Fatal(err)
			}
			var backups []string
			t.Cleanup(func() {
				for _, path := range append(backups, dest) {
					err := os.Chmod(filepath.Join(path, oldDir), 0o755)
					if err != nil && !errors.Is(err, os.ErrNotExist) {
						t.Error(err)
					}
				}
			})
			if err := os.Chmod(filepath.Join(dest, oldDir), tc.mode); err != nil {
				t.Fatal(err)
			}
			var done int
			res, err := c.Download(t.Context(), DownloadParams{
				OrganizationID: org, DevSessionID: ds.GetDevSessionId(), GuestPath: "out", LocalPath: dest,
				Progress: func(p TransferProgress) {
					if p.Phase == TransferDone {
						done++
					}
				},
			})
			backupDirs, globErr := filepath.Glob(filepath.Join(parent, ".tide-old-*"))
			if globErr != nil {
				t.Fatal(globErr)
			}
			for _, dir := range backupDirs {
				backups = append(backups, filepath.Join(dir, "previous"))
			}
			if err != nil || res == nil || res.LocalPath != dest || !res.Directory ||
				res.Files != 1 || res.Bytes != 3 || done != 1 {
				t.Fatalf("download = %+v, %v, completion events %d", res, err, done)
			}
			if contents, err := os.ReadFile(filepath.Join(dest, "new.txt")); err != nil || string(contents) != "new" {
				t.Fatalf("new content = %q, %v", contents, err)
			}
			if !cleanupDenied {
				if len(backups) != 0 || !slices.Equal(res.Warnings, []string{extractionWarning}) {
					t.Fatalf("backups = %v, warnings = %v", backups, res.Warnings)
				}
				return
			}
			if len(backups) != 1 || len(res.Warnings) != 2 || res.Warnings[0] != extractionWarning ||
				!strings.Contains(res.Warnings[1], strconv.Quote(backups[0])) ||
				!strings.Contains(res.Warnings[1], os.ErrPermission.Error()) {
				t.Fatalf("backups = %v, warnings = %v", backups, res.Warnings)
			}
			if contents, err := os.ReadFile(filepath.Join(backups[0], oldDir, "old.txt")); err != nil || string(contents) != "old" {
				t.Fatalf("old content = %q, %v", contents, err)
			}
		})
	}
}

func TestDownloadAtFilenameLimit(t *testing.T) {
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	ds := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_LINUX)
	ws := ds.GetWorkspacePath()
	if err := os.Mkdir(filepath.Join(ws, "tree"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"file", "tree/new.txt"} {
		if err := os.WriteFile(filepath.Join(ws, path), []byte("new"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Use the full 255-byte basename allowed by common POSIX filesystems.
	name := strings.Repeat("a", 255)
	for _, tc := range []struct {
		name, guest, oldPath, newPath string
		failRead                      bool
	}{
		{name: "replace file", guest: "file", oldPath: ".", newPath: "."},
		{name: "replace directory", guest: "tree", oldPath: "old.txt", newPath: "new.txt"},
		{name: "install file", guest: "file", newPath: "."},
		{name: "failed transfer", guest: "file", oldPath: ".", failRead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			dest := filepath.Join(parent, name)
			if tc.oldPath != "" {
				old := filepath.Join(dest, tc.oldPath)
				if err := os.MkdirAll(filepath.Dir(old), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(old, []byte("old"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.failRead {
				srv.Fail("/delino.tide.v1.DevSessionService/ReadDevSessionFile",
					connect.NewError(connect.CodePermissionDenied, errors.New("transfer stopped")))
			}
			res, err := c.Download(t.Context(), DownloadParams{
				OrganizationID: org, DevSessionID: ds.GetDevSessionId(), GuestPath: tc.guest, LocalPath: dest,
			})
			if tc.failRead {
				if connect.CodeOf(err) != connect.CodePermissionDenied {
					t.Fatalf("failed transfer = %v", err)
				}
				if contents, err := os.ReadFile(dest); err != nil || string(contents) != "old" {
					t.Fatalf("destination after failed transfer = %q, %v", contents, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if res.LocalPath != dest || res.Directory != (tc.guest == "tree") {
					t.Fatalf("download result = %+v", res)
				}
				if contents, err := os.ReadFile(filepath.Join(dest, tc.newPath)); err != nil || string(contents) != "new" {
					t.Fatalf("downloaded content = %q, %v", contents, err)
				}
				if tc.oldPath == "old.txt" {
					if _, err := os.Lstat(filepath.Join(dest, tc.oldPath)); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("old directory entry remains: %v", err)
					}
				}
			}
			entries, err := os.ReadDir(parent)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != name {
				t.Fatalf("unexpected destination parent entries: %v", entries)
			}
		})
	}
}

func TestReplacePathRollback(t *testing.T) {
	for _, tc := range []struct {
		name         string
		blockRestore bool
	}{
		{name: "restore destination"},
		{name: "retain backup", blockRestore: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			dest := filepath.Join(parent, "result")
			staged := filepath.Join(parent, "missing")
			if err := os.WriteFile(dest, []byte("old"), 0o644); err != nil {
				t.Fatal(err)
			}

			var backup string
			rename := func(from, to string) error {
				if from == dest {
					backup = to
				}
				if from == staged && tc.blockRestore {
					// A directory appears after the old file moved aside.
					if err := os.Mkdir(dest, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				return os.Rename(from, to)
			}
			warning, err := replacePath(staged, dest, rename)
			if warning != "" || !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("publication warning = %q, error = %v", warning, err)
			}
			if backup == "" {
				t.Fatal("the destination was not backed up")
			}
			old := dest
			if tc.blockRestore {
				if !strings.Contains(err.Error(), strconv.Quote(backup)) {
					t.Fatalf("error does not report the backup path %q: %v", backup, err)
				}
				old = backup
			} else if _, err := os.Lstat(filepath.Dir(backup)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("backup directory remains: %v", err)
			}
			if contents, err := os.ReadFile(old); err != nil || string(contents) != "old" {
				t.Fatalf("old content = %q, %v", contents, err)
			}
		})
	}
}

// TestDownloadResumeKeepsReadID checks that a read that continues after a
// stream ended early uses the same read_id, so it reads the same snapshot
// also when the guest file changes in between.
func TestDownloadResumeKeepsReadID(t *testing.T) {
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	srv.ReadStreamChunkLimit = 1
	ds := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_LINUX)
	guestFile := filepath.Join(ds.GetWorkspacePath(), "data.bin")
	size := 2*ChunkSize + 10
	if err := os.WriteFile(guestFile, bytes.Repeat([]byte("a"), size), 0o644); err != nil {
		t.Fatal(err)
	}
	changed := false
	local := filepath.Join(t.TempDir(), "data.bin")
	_, err := c.Download(context.Background(), DownloadParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), GuestPath: "data.bin", LocalPath: local, Progress: func(p TransferProgress) {
		if p.Phase == TransferDownloading && p.BytesDone > 0 && !changed {
			// A same-size replacement after the first stream.
			changed = true
			if err := os.WriteFile(guestFile, bytes.Repeat([]byte("b"), size), 0o644); err != nil {
				t.Error(err)
			}
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(local); !bytes.Equal(b, bytes.Repeat([]byte("a"), size)) {
		t.Fatal("the resumed download joined two versions of the file")
	}
	reads := srv.ReadRequests()
	var ids []string
	for _, r := range reads {
		if r.GetPath() == guestFile {
			ids = append(ids, r.GetReadId())
		}
	}
	if len(ids) != 3 || ids[0] != ids[1] || ids[1] != ids[2] {
		t.Fatalf("read_ids of the download = %v, want one read_id in 3 calls", ids)
	}
}

// TestSyncArchiveRejectsChangedFiles checks that the archive fails when a
// file changed after it was hashed: same size with other bytes, or more
// bytes.
func TestSyncArchiveRejectsChangedFiles(t *testing.T) {
	needPOSIX(t)
	for name, content := range map[string]string{"same size": "bbbb", "grown": "aaaaa", "shrunk": "aaa"} {
		dir := newRepo(t, map[string]string{"f.txt": "aaaa"}, nil, "")
		local, err := listLocalFiles(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		if err := writeSyncArchive(context.Background(), &b, dir, local.files, false); err == nil || !strings.Contains(err.Error(), "changed during the sync") {
			t.Errorf("%s: archive of a changed file = %v", name, err)
		}
	}
	dir := newRepo(t, map[string]string{"f.txt": "aaaa"}, nil, "")
	local, err := listLocalFiles(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := writeSyncArchive(context.Background(), &b, dir, local.files, false); err != nil {
		t.Fatalf("archive of an unchanged file = %v", err)
	}
}

// tarOf builds an archive from headers; regular entries get body content.
func tarOf(t *testing.T, entries ...*tar.Header) *bytes.Reader {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, h := range entries {
		body := ""
		if h.Typeflag == tar.TypeReg {
			body = strings.Repeat("x", int(h.Size))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if body != "" {
			_, _ = tw.Write([]byte(body))
		}
	}
	_ = tw.Close()
	return bytes.NewReader(b.Bytes())
}

func TestExtractRejectsPathTraversal(t *testing.T) {
	ctx := context.Background()
	extractor := archiveExtractor{fileLimit: 100, totalLimit: 1000, link: (*os.Root).Link}
	cases := map[string][]*tar.Header{
		"absolute":        {{Typeflag: tar.TypeReg, Name: "/etc/passwd", Size: 1, Mode: 0o644}},
		"parent":          {{Typeflag: tar.TypeReg, Name: "../escape", Size: 1, Mode: 0o644}},
		"nested parent":   {{Typeflag: tar.TypeReg, Name: "a/../../escape", Size: 1, Mode: 0o644}},
		"link outside":    {{Typeflag: tar.TypeSymlink, Name: "l", Linkname: "../../etc"}},
		"absolute link":   {{Typeflag: tar.TypeSymlink, Name: "l", Linkname: "/etc"}},
		"hard link out":   {{Typeflag: tar.TypeLink, Name: "h", Linkname: "../x"}},
		"hard link miss":  {{Typeflag: tar.TypeLink, Name: "h", Linkname: "missing"}},
		"through symlink": {{Typeflag: tar.TypeSymlink, Name: "l", Linkname: "."}, {Typeflag: tar.TypeReg, Name: "l/../../x", Size: 1, Mode: 0o644}},
	}
	for name, entries := range cases {
		parent := t.TempDir()
		dir := filepath.Join(parent, "dest")
		_ = os.Mkdir(dir, 0o755)
		if _, err := extractor.extractArchive(ctx, tarOf(t, entries...), dir); err == nil {
			t.Errorf("%s: extraction accepted the archive", name)
		}
		if _, err := os.Stat(filepath.Join(parent, "escape")); err == nil {
			t.Errorf("%s: a file escaped the destination", name)
		}
	}

	// Links that pass the text check but leave the destination once the
	// other links of the archive are resolved.
	for name, entries := range map[string][]*tar.Header{
		"link in a linked directory": {
			{Typeflag: tar.TypeDir, Name: "a/", Mode: 0o755},
			{Typeflag: tar.TypeSymlink, Name: "a/b", Linkname: ".."},
			{Typeflag: tar.TypeSymlink, Name: "a/b/l", Linkname: "../x"},
		},
		"target through a link": {
			{Typeflag: tar.TypeDir, Name: "a/", Mode: 0o755},
			{Typeflag: tar.TypeSymlink, Name: "a/b", Linkname: ".."},
			{Typeflag: tar.TypeSymlink, Name: "l", Linkname: "a/b/../../x"},
		},
		"two links in one directory": {
			{Typeflag: tar.TypeDir, Name: "a/", Mode: 0o755},
			{Typeflag: tar.TypeSymlink, Name: "a/p", Linkname: ".."},
			{Typeflag: tar.TypeSymlink, Name: "a/q", Linkname: "p/.."},
		},
	} {
		dir := filepath.Join(t.TempDir(), "dest")
		_ = os.Mkdir(dir, 0o755)
		if _, err := extractor.extractArchive(ctx, tarOf(t, entries...), dir); err == nil || !strings.Contains(err.Error(), "resolves outside") {
			t.Errorf("%s: extraction = %v", name, err)
		}
	}
	// Chained links that stay inside are kept.
	inside := t.TempDir()
	if _, err := extractor.extractArchive(ctx, tarOf(t,
		&tar.Header{Typeflag: tar.TypeReg, Name: "d/e/f", Size: 1, Mode: 0o644},
		&tar.Header{Typeflag: tar.TypeSymlink, Name: "l1", Linkname: "d/e"},
		&tar.Header{Typeflag: tar.TypeSymlink, Name: "l2", Linkname: "l1/../e/f"},
		&tar.Header{Typeflag: tar.TypeSymlink, Name: "dangling", Linkname: "l1/missing/x"},
	), inside); err != nil {
		t.Fatalf("links inside: %v", err)
	}

	dir := t.TempDir()
	st, err := extractor.extractArchive(ctx, tarOf(t,
		&tar.Header{Typeflag: tar.TypeDir, Name: "./", Mode: 0o755},
		&tar.Header{Typeflag: tar.TypeReg, Name: "./a/f", Size: 3, Mode: 0o644},
		&tar.Header{Typeflag: tar.TypeLink, Name: "./a/g", Linkname: "./a/f"},
		&tar.Header{Typeflag: tar.TypeSymlink, Name: "./a/l", Linkname: "f"},
		&tar.Header{Typeflag: tar.TypeFifo, Name: "./fifo"},
	), dir)
	if err != nil || st.files != 3 || len(st.warnings) != 1 {
		t.Fatalf("valid archive: %+v %v", st, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a/g")); string(b) != "xxx" {
		t.Fatal("hard link content")
	}

	// Entry limits are checked during extraction.
	extractor.fileLimit, extractor.totalLimit = 10, 15
	var le *LimitError
	if _, err := extractor.extractArchive(ctx, tarOf(t, &tar.Header{Typeflag: tar.TypeReg, Name: "big", Size: 11, Mode: 0o644}), t.TempDir()); !errors.As(err, &le) {
		t.Fatalf("file limit: %v", err)
	}
	if _, err := extractor.extractArchive(ctx, tarOf(t,
		&tar.Header{Typeflag: tar.TypeReg, Name: "a", Size: 8, Mode: 0o644},
		&tar.Header{Typeflag: tar.TypeReg, Name: "b", Size: 8, Mode: 0o644},
	), t.TempDir()); !errors.As(err, &le) || le.Total != 16 {
		t.Fatalf("total limit: %v", err)
	}
}

func TestExtractHardLinkLimits(t *testing.T) {
	extractor := archiveExtractor{fileLimit: 8, totalLimit: 12}
	st := &extractStats{bytes: 4}
	called := false
	write := func() error {
		called = true
		return nil
	}
	var limit *LimitError
	err := extractor.writeFile(st, "copy", 9, write)
	if !errors.As(err, &limit) || !slices.Equal(limit.Paths, []PathSize{{Path: "copy", Size: 9}}) || called || st.bytes != 4 {
		t.Fatalf("per-file limit: error = %v, writer called = %t, bytes = %d", err, called, st.bytes)
	}
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.WriteFile("source", []byte("too large"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = extractor.copyFile(root, st, "source", "copy")
	if !errors.As(err, &limit) || !slices.Equal(limit.Paths, []PathSize{{Path: "copy", Size: 9}}) || st.bytes != 4 {
		t.Fatalf("fallback per-file limit: error = %v, bytes = %d", err, st.bytes)
	}
	if _, err := root.Stat("copy"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("rejected copy exists: stat error = %v", err)
	}
	err = extractor.writeFile(st, "copy", 8, func() error { return fs.ErrPermission })
	if !errors.Is(err, fs.ErrPermission) || st.bytes != 4 {
		t.Fatalf("write failure: error = %v, bytes = %d", err, st.bytes)
	}
	err = extractor.writeFile(st, "copy", 8, write)
	if err != nil || !called || st.bytes != 12 {
		t.Fatalf("exact limit: error = %v, writer called = %t, bytes = %d", err, called, st.bytes)
	}
	called = false
	err = extractor.writeFile(st, "next-copy", 1, write)
	if !errors.As(err, &limit) || limit.Total != 13 || called || st.bytes != 12 {
		t.Fatalf("total limit: error = %v, writer called = %t, bytes = %d", err, called, st.bytes)
	}
}

func TestLinkFailureScan(t *testing.T) {
	var s linkFailureScan
	for _, chunk := range []string{"noise\nTIDE-LINK-", "FAILED a\\l1\r\n", "TIDE-LINK-FAILED b\n", "TIDE-LINK-FAILED last"} {
		s.write([]byte(chunk))
	}
	s.end()
	if !slices.Equal(s.paths, []string{`a\l1`, "b", "last"}) || s.lost {
		t.Fatalf("scan = %q lost %v", s.paths, s.lost)
	}
	var long linkFailureScan
	long.write([]byte(linkFailedPrefix + strings.Repeat("x", guestOutputLimit) + "\nTIDE-LINK-FAILED c\n"))
	long.end()
	if !slices.Equal(long.paths, []string{"c"}) || !long.lost {
		t.Fatalf("long line scan = %q lost %v", long.paths, long.lost)
	}
}

// TestRunGuestReadsEveryLinkFailure checks that the link failures after
// the captured start of stdout are not lost.
func TestRunGuestReadsEveryLinkFailure(t *testing.T) {
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	ds := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_LINUX)
	saved := posixScripts[opApply]
	t.Cleanup(func() { posixScripts[opApply] = saved })
	const n = 5000
	posixScripts[opApply] = `i=0; while [ $i -lt 5000 ]; do echo "TIDE-LINK-FAILED dir/link-$i"; i=$((i+1)); done`
	r, err := c.runGuest(context.Background(), ds, opApply, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.stdout) > guestOutputLimit+ChunkSize || len(r.linkFailures) != n || r.outputLost || r.linkFailures[n-1] != "dir/link-4999" {
		t.Fatalf("captured %d bytes, %d link failures, lost %v", len(r.stdout), len(r.linkFailures), r.outputLost)
	}
}

func TestLinkOutcome(t *testing.T) {
	links := []localFile{{rel: "a/l1"}, {rel: "l2"}, {rel: "l3"}}
	failed, warnings := linkOutcome(links, &guestResult{linkFailures: []string{`a\l1`, "a/l1", "not-sent"}})
	if len(failed) != 1 || !failed["a/l1"] || len(warnings) != 1 {
		t.Fatalf("failed %v warnings %q", failed, warnings)
	}
	// Incomplete output: no link counts as created.
	failed, warnings = linkOutcome(links, &guestResult{linkFailures: []string{"l2"}, outputLost: true})
	if len(failed) != 3 || len(warnings) != 3 {
		t.Fatalf("incomplete output: failed %v warnings %q", failed, warnings)
	}
}

func TestLinkOutcomeInSyncManifest(t *testing.T) {
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	ds := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_WINDOWS)
	dir := newRepo(t, nil, nil, "")
	if err := os.Symlink("target", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	// Emulate the Windows command results on the fake POSIX guest.
	bin := t.TempDir()
	script := `#!/bin/sh
set -eu
if [ -n "${TIDE_SYNC_FORCE-}" ]; then
    mkdir "$TIDE_SYNC_LOCK"
elif [ -n "${TIDE_SYNC_WORKSPACE-}" ]; then
    test -s "$TIDE_SYNC_LINK_LIST"
    echo 'TIDE-LINK-FAILED link'
else
    rm -f "$TIDE_SYNC_LINK_LIST"
    rmdir "$TIDE_SYNC_LOCK"
fi
`
	if err := os.WriteFile(filepath.Join(bin, "powershell.exe"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	params := SyncParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), LocalDir: dir}
	res, err := c.Sync(context.Background(), params)
	if err != nil || res.Uploaded != 0 || !slices.Equal(res.Warnings, []string{"link: the guest could not create the symbolic link"}) {
		t.Fatalf("sync with a failed Windows link = %+v, %v", res, err)
	}
	if m := guestManifest(t, ds); len(m.Files) != 0 {
		t.Fatalf("manifest contains the failed link: %+v", m)
	}
}

func TestGuestCommandsAndPaths(t *testing.T) {
	cmd, err := guestCommand(tidev1.GuestOs_GUEST_OS_WINDOWS, opApply)
	if err != nil || cmd[0] != "powershell.exe" || cmd[len(cmd)-2] != "-EncodedCommand" {
		t.Fatalf("windows command %v %v", cmd, err)
	}
	if !strings.Contains(windowsScripts[opApply], `System32\tar.exe`) {
		t.Fatal("the Windows apply script does not use tar.exe")
	}
	cmd, _ = guestCommand(tidev1.GuestOs_GUEST_OS_LINUX, opLock)
	if cmd[0] != "sh" || cmd[1] != "-c" {
		t.Fatalf("posix command %v", cmd)
	}
	if _, err := guestCommand(tidev1.GuestOs_GUEST_OS_UNSPECIFIED, opLock); err == nil {
		t.Fatal("unspecified guest OS accepted")
	}
	win := &tidev1.DevSession{GuestOs: tidev1.GuestOs_GUEST_OS_WINDOWS, WorkspacePath: `C:\work\`}
	if got := guestSibling(win, lockSuffix); got != `C:\work.tide-sync.lock` {
		t.Fatalf("windows sibling %q", got)
	}
	if got := guestResolve(win, "out/bin"); got != `C:\work\out\bin` {
		t.Fatalf("windows resolve %q", got)
	}
	if got := guestResolve(win, `D:\x`); got != `D:\x` {
		t.Fatalf("windows absolute %q", got)
	}
	lin := &tidev1.DevSession{GuestOs: tidev1.GuestOs_GUEST_OS_LINUX, WorkspacePath: "/home/dev/work"}
	if got := guestSibling(lin, manifestSuffix); got != "/home/dev/work.tide-sync.json" {
		t.Fatalf("linux sibling %q", got)
	}
	if got := guestResolve(lin, "/tmp/x"); got != "/tmp/x" {
		t.Fatalf("linux absolute %q", got)
	}
}

// Keep the fake import used when tests are filtered.
var _ = tidetest.DefaultAPIKey
