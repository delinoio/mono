package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/go/sandrun"
	"github.com/delinoio/mono/go/sandrun/sandruntest"
	v1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func require(ok bool, message string, args ...any) {
	if !ok {
		log.Fatalf(message, args...)
	}
}

func read(path string) []byte {
	data, err := os.ReadFile(path)
	must(err)
	return data
}

func write(path, body string) {
	must(os.WriteFile(path, []byte(body), 0o644))
}

func git(dir string, args ...string) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require(err == nil, "git %q: %v: %s", args, err, out)
}

func checkCleanup(ws string) {
	_, err := os.Lstat(ws + ".sandrun-sync.lock")
	require(errors.Is(err, os.ErrNotExist), "expected lock removal, got %v", err)
	matches, err := filepath.Glob(ws + ".sandrun-sync-*")
	must(err)
	require(len(matches) == 0, "sync inputs left: %q", matches)
}

func main() {
	require(len(os.Args) == 2 && (os.Args[1] == "case-sensitive" || os.Args[1] == "case-insensitive"),
		"usage: sync-retry-probe case-sensitive|case-insensitive")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	root, err := os.MkdirTemp("", "sandrun-sdk-sync-retry-")
	must(err)
	defer os.RemoveAll(root)
	src, ws := filepath.Join(root, "source"), filepath.Join(root, "work")
	must(os.Mkdir(src, 0o755))
	must(os.Mkdir(ws, 0o755))
	git(src, "init", "-q")
	write(filepath.Join(src, "Assets"), "old")
	git(src, "add", "--", "Assets")
	manifest := ws + ".sandrun-sync.json"

	srv := sandruntest.New()
	defer srv.Close()
	org := srv.AddOrganization("sync retry probe").GetOrganizationId()
	profile := srv.AddDevSessionProfile(org, "sync probe", v1.GuestOs_GUEST_OS_MACOS, ws)
	client, err := sandrun.New(sandrun.Options{BaseURL: srv.URL, APIKey: srv.AccessToken})
	must(err)
	sess, err := client.DevSessions().CreateDevSession(ctx, connect.NewRequest(&v1.CreateDevSessionRequest{
		OrganizationId: org, RunnerProfileId: profile.GetRunnerProfileId(),
	}))
	must(err)
	params := sandrun.SyncParams{OrganizationID: org, DevSessionID: sess.Msg.GetDevSession().GetDevSessionId(), LocalDir: src}
	_, err = client.Sync(ctx, params)
	must(err)
	checkCleanup(ws)
	before := read(manifest)
	require(string(read(filepath.Join(ws, "Assets"))) == "old", "initial sync did not write the old file")
	_, err = os.Lstat(filepath.Join(ws, "assets"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		must(err)
	}
	caseSensitive := errors.Is(err, os.ErrNotExist)
	fmt.Printf("guest workspace keeps Assets and assets distinct: %t\n", caseSensitive)
	require(caseSensitive == (os.Args[1] == "case-sensitive"), "workspace case behavior differs from requested mode")

	git(src, "rm", "-q", "-f", "--", "Assets")
	must(os.Mkdir(filepath.Join(src, "assets"), 0o755))
	const child = "assets/new.txt"
	write(filepath.Join(src, child), "new")
	git(src, "add", "--", child)
	srv.BeforeExec = func(req *v1.ExecDevSessionRequest) {
		env := req.GetEnv()
		if env["SANDRUN_SYNC_WORKSPACE"] != "" && env["SANDRUN_SYNC_ARCHIVE"] != "" {
			srv.Fail(sandrunv1connect.DevSessionServiceWriteDevSessionFileProcedure, connect.NewError(connect.CodePermissionDenied, errors.New("injected manifest write failure")))
		}
	}
	_, err = client.Sync(ctx, params)
	srv.BeforeExec = nil
	require(connect.CodeOf(err) == connect.CodePermissionDenied, "expected manifest write error, got %v", err)
	require(string(read(filepath.Join(ws, child))) == "new", "apply did not replace the former file")
	require(bytes.Equal(before, read(manifest)), "failed manifest write changed the old manifest")
	checkCleanup(ws)
	fmt.Println("first apply replaced the old file, then manifest publication failed and cleanup completed")

	targetChild := filepath.Join(ws, child)
	write(targetChild, "guest edit")
	sentinel := filepath.Join(ws, "assets", "guest.txt")
	write(sentinel, "keep")
	if caseSensitive {
		oldDir := filepath.Join(ws, "Assets")
		must(os.Mkdir(oldDir, 0o755))
		blocker := filepath.Join(oldDir, "guest-only.txt")
		write(blocker, "keep blocker")
		_, err := client.Sync(ctx, params)
		require(connect.CodeOf(err) == connect.CodeInternal && strings.Contains(err.Error(), "guest apply command"), "expected strict guest apply error, got %v", err)
		require(string(read(blocker)) == "keep blocker", "strict deletion changed the distinct directory")
		require(string(read(targetChild)) == "guest edit", "unexpected archive extraction after strict deletion failed")
		require(bytes.Equal(before, read(manifest)), "strict deletion failure changed the manifest")
		checkCleanup(ws)
		fmt.Println("distinct old directory caused strict deletion failure and kept its contents and the old manifest")
		must(os.Remove(blocker))
		must(os.Remove(oldDir))
	}
	res, err := client.Sync(ctx, params)
	must(err)
	require(res.Uploaded == 1 && res.Deleted == 1, "unexpected retry result: %+v", res)
	require(string(read(targetChild)) == "new", "retry did not restore the source child")
	require(string(read(sentinel)) == "keep", "retry changed guest-only data")
	var m struct {
		Version int
		Files   map[string]json.RawMessage
	}
	must(json.Unmarshal(read(manifest), &m))
	require(m.Version == 1 && len(m.Files) == 1 && m.Files[child] != nil, "unexpected retry manifest: %+v", m)
	checkCleanup(ws)
	fmt.Println("normal retry restored the source child, kept guest-only data, published the new manifest and released the lock")
}
