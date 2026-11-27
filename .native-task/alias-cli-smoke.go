package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/go/sandrun"
	"github.com/delinoio/mono/go/sandrun/sandruntest"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
)

func check(err error) {
	if err != nil {
		panic(err)
	}
}

func verify(ok bool, situation string) {
	if !ok {
		panic(situation)
	}
}

func run(dir, name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, os.Stdout, os.Stderr
	check(cmd.Run())
}

func read(path string) string {
	body, err := os.ReadFile(path)
	check(err)
	return string(body)
}

func sync(dir, cli, session string) (sandrun.SyncResult, string, error) {
	cmd := exec.Command(cli, "--output", "json", "session", "sync", session, dir)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	var result sandrun.SyncResult
	if err == nil {
		check(json.Unmarshal(out, &result))
	}
	return result, string(out), err
}

func main() {
	root, err := os.MkdirTemp("", "sandrun-cli-workspaces-")
	check(err)
	defer os.RemoveAll(root)
	cli, err := filepath.Abs(os.Args[1])
	check(err)
	fake := sandruntest.New()
	defer fake.Close()
	org := fake.AddOrganization("alias-smoke").GetOrganizationId()
	check(os.Setenv("SANDRUN_API_URL", fake.URL))
	check(os.Setenv("SANDRUN_API_KEY", fake.APIKey))
	check(os.Setenv("SANDRUN_ORG", org))
	check(os.Setenv("SANDRUN_CONFIG_DIR", filepath.Join(root, "config")))
	check(os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull))
	check(os.Setenv("GIT_CONFIG_NOSYSTEM", "1"))
	client, err := sandrun.New(sandrun.Options{BaseURL: fake.URL, APIKey: fake.APIKey})
	check(err)

	osys := sandrunv1.GuestOs_GUEST_OS_MACOS
	cases := []struct{ name, old, incoming string }{{"case", "Assets", "assets"}}
	if runtime.GOOS == "windows" {
		osys = sandrunv1.GuestOs_GUEST_OS_WINDOWS
	} else {
		cases = append(cases, struct{ name, old, incoming string }{"normalization", "caf\u00e9", "cafe\u0301"})
	}
	for _, tc := range cases {
		for _, blocker := range []bool{false, true} {
			parent := filepath.Join(root, fmt.Sprintf("%s-%t", tc.name, blocker))
			dir, ws := filepath.Join(parent, "source"), filepath.Join(parent, "workspace")
			oldFile := filepath.Join(tc.old, "nested", "old.txt")
			guestOnly := filepath.Join(ws, tc.old, "nested", "guest.txt")
			incoming := filepath.Join(ws, tc.incoming)
			check(os.MkdirAll(filepath.Join(dir, tc.old, "nested"), 0o755))
			check(os.Mkdir(ws, 0o755))
			check(os.WriteFile(filepath.Join(dir, oldFile), []byte("old"), 0o644))
			run(dir, "git", "init", "-q")
			run(dir, "git", "config", "core.precomposeUnicode", "false")
			run(dir, "git", "add", "--", filepath.ToSlash(oldFile))
			profile := fake.AddDevSessionProfile(org, filepath.Base(parent), osys, ws)
			session, err := client.DevSessions().CreateDevSession(context.Background(), connect.NewRequest(&sandrunv1.CreateDevSessionRequest{
				OrganizationId:  org,
				RunnerProfileId: profile.GetRunnerProfileId(),
			}))
			check(err)
			id := session.Msg.GetDevSession().GetDevSessionId()
			first, out, err := sync(dir, cli, id)
			verify(err == nil, "initial sync failed: "+out)
			verify(first.Uploaded == 1 && first.Deleted == 0, "initial sync counts")
			verify(read(filepath.Join(ws, oldFile)) == "old", "initial file bytes")
			before := read(ws + ".sandrun-sync.json")
			info, err := os.Stat(incoming)
			verify(err == nil || errors.Is(err, os.ErrNotExist), "could not check filename lookup")
			aliases := err == nil && info.IsDir()
			if tc.name == "normalization" && runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
				verify(aliases, "native workspace must resolve both spellings to the old directory")
			}
			if runtime.GOOS == "linux" {
				verify(!aliases, "Linux control must distinguish spellings")
			}

			if blocker {
				check(os.WriteFile(guestOnly, []byte("guest"), 0o644))
			}
			run(dir, "git", "rm", "-q", "-r", "-f", "--", tc.old)
			check(os.WriteFile(filepath.Join(dir, tc.incoming), []byte("new"), 0o644))
			run(dir, "git", "add", "--", tc.incoming)

			replacement, out, err := sync(dir, cli, id)
			result := "one upload and deletion, incoming manifest spelling, and guest-only bytes kept if present"
			if aliases && blocker {
				verify(err != nil && strings.Contains(out, "guest apply command"), "expected apply error: "+out)
				verify(read(ws+".sandrun-sync.json") == before, "the old manifest should be unchanged")
				result = "apply failed, guest-only bytes and old manifest kept"
			} else {
				verify(err == nil, "replacement sync failed: "+out)
				verify(replacement.Uploaded == 1 && replacement.Deleted == 1 && len(replacement.Warnings) == 0, "replacement sync counts or warnings")
				verify(read(incoming) == "new", "replacement bytes")
				manifest := struct{ Files map[string]json.RawMessage }{}
				check(json.Unmarshal([]byte(read(ws+".sandrun-sync.json")), &manifest))
				verify(len(manifest.Files) == 1 && manifest.Files[tc.incoming] != nil, "manifest must hold only the incoming spelling")
			}
			if blocker {
				verify(read(guestOnly) == "guest", "guest-only bytes")
			}
			if !aliases {
				_, err := os.ReadDir(filepath.Join(ws, tc.old, "nested"))
				check(err)
			}
			if !aliases || blocker {
				_, err = os.Stat(filepath.Join(ws, oldFile))
				verify(errors.Is(err, os.ErrNotExist), "old tracked leaf should be gone")
			}
			matches, err := filepath.Glob(ws + ".sandrun-sync-*")
			check(err)
			verify(len(matches) == 0, "sync temporary inputs must be removed")
			_, err = os.Stat(ws + ".sandrun-sync.lock")
			verify(errors.Is(err, os.ErrNotExist), "sync lock must be removed")
			fmt.Printf("%s blocker=%t alias=%t: %s, no temporary inputs or lock\n", tc.name, blocker, aliases, result)
		}
	}
}
