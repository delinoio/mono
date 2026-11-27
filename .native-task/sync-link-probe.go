// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/go/sandrun"
	"github.com/delinoio/mono/go/sandrun/sandruntest"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

const manifestSuffix = ".sandrun-sync.json"
const malformedTarget = "dest-\xff"
const validTarget = "dest-\uFFFD"

type manifestEntry struct {
	Size int    `json:"size"`
	Hash string `json:"sha256"`
	Mode uint32 `json:"mode"`
}

type manifest struct {
	Version        int                      `json:"version"`
	Files          map[string]manifestEntry `json:"files"`
	PathsValidated bool                     `json:"paths_validated"`
}

func assert(ok bool, message string, args ...any) {
	if !ok {
		panic(fmt.Sprintf(message, args...))
	}
}

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}

func entry(target string) manifestEntry {
	return manifestEntry{Size: len(target), Hash: fmt.Sprintf("%x", sha256.Sum256([]byte(target))), Mode: 0o120000}
}

func main() {
	log.SetFlags(0)
	assert(len(os.Args) == 2, "usage: sync-link-probe <sandrun binary>")
	binary := must(filepath.Abs(os.Args[1]))
	root := must(os.MkdirTemp("", "sandrun-link-probe-"))
	defer os.RemoveAll(root)
	src := filepath.Join(root, "src")
	check(os.Mkdir(src, 0o755))

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	git := func(args ...string) string {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", src}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		assert(err == nil, "git %v: %v: %s", args, err, out)
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("config", "core.excludesFile", os.DevNull)
	git("config", "core.symlinks", "false")
	git("config", "core.autocrlf", "false")
	link := filepath.Join(src, "link")
	check(os.WriteFile(link, []byte(malformedTarget), 0o644))
	git("add", "--", "link")
	git("update-index", "--cacheinfo", "120000,"+git("rev-parse", ":link")+",link")
	assert(strings.HasPrefix(git("ls-files", "--stage", "--", "link"), "120000 "), "missing Git symlink index entry")

	srv := sandruntest.New()
	defer srv.Close()
	type stage struct {
		body []byte
		err  error
	}
	staged := make(chan stage, 1)
	srv.BeforeExec = func(req *sandrunv1.ExecDevSessionRequest) {
		env := req.GetEnv()
		if path := env["SANDRUN_SYNC_LINK_LIST"]; path != "" && env["SANDRUN_SYNC_WORKSPACE"] != "" {
			body, err := os.ReadFile(path)
			staged <- stage{body, err}
		}
	}
	org := srv.AddOrganization("encoding-fixture").GetOrganizationId()
	client := must(sandrun.New(sandrun.Options{BaseURL: srv.URL, APIKey: srv.AccessToken}))
	newSession := func(name string, guestOS sandrunv1.GuestOs) (string, string) {
		ws := filepath.Join(root, name, "work")
		check(os.MkdirAll(ws, 0o755))
		profile := srv.AddDevSessionProfile(org, name, guestOS, ws)
		res := must(client.DevSessions().CreateDevSession(ctx, connect.NewRequest(&sandrunv1.CreateDevSessionRequest{
			OrganizationId: org, RunnerProfileId: profile.GetRunnerProfileId(),
		})))
		return res.Msg.GetDevSession().GetDevSessionId(), ws
	}
	sync := func(id string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, binary, "--api-url", srv.URL, "--org", org, "--output", "json", "--no-color", "session", "sync", id, src)
		cmd.Env = append(os.Environ(), "SANDRUN_API_KEY="+srv.APIKey, "SANDRUN_CONFIG_DIR="+filepath.Join(root, "config"))
		return cmd.CombinedOutput()
	}

	windows, ws := newSession("windows", sandrunv1.GuestOs_GUEST_OS_WINDOWS)
	manifestPath := ws + manifestSuffix
	seed := must(json.Marshal(manifest{Version: 1, Files: map[string]manifestEntry{"link": entry(malformedTarget)}, PathsValidated: true}))
	for _, matched := range []bool{false, true} {
		if matched {
			check(os.WriteFile(manifestPath, seed, 0o644))
		}
		out, err := sync(windows)
		var exit *exec.ExitError
		assert(errors.As(err, &exit) && exit.ExitCode() == 1 &&
			bytes.Contains(out, []byte(`failed_precondition: sandrun: symbolic link "link" has a target with invalid UTF-8`)) &&
			!bytes.Contains(out, []byte("dest-")), "invalid target with matching manifest=%t: %v: %s", matched, err, out)
		execs := srv.Calls(sandrunv1connect.DevSessionServiceExecDevSessionProcedure)
		writes := srv.Calls(sandrunv1connect.DevSessionServiceWriteDevSessionFileProcedure)
		assert(execs == 0 && writes == 0, "unexpected guest activity: execs=%d writes=%d", execs, writes)
		body, err := os.ReadFile(manifestPath)
		if matched {
			assert(err == nil && bytes.Equal(body, seed), "the seeded manifest changed: %v", err)
		} else {
			assert(errors.Is(err, os.ErrNotExist), "unexpected manifest after rejecting new link: %v", err)
		}
		children := must(os.ReadDir(ws))
		assert(len(children) == 0, "workspace changed after rejection")
		log.Printf("Windows guest, matching manifest=%t: exit=1, failed_precondition for link, execs=%d, writes=%d, manifest preserved", matched, execs, writes)
	}

	if runtime.GOOS == "windows" {
		check(os.WriteFile(link, []byte(validTarget), 0o644))
		check(os.WriteFile(filepath.Join(ws, validTarget), nil, 0o644))
		out, err := sync(windows)
		assert(err == nil, "valid target sync: %v: %s", err, out)
		select {
		case got := <-staged:
			want := "link\t" + validTarget + "\n"
			assert(got.err == nil && string(got.body) == want, "staged bytes=%x, want %x: %v", got.body, want, got.err)
			log.Printf("Native Windows guest: PowerShell apply reached, staged link list=%x", got.body)
		default:
			panic("valid target failed to reach guest apply")
		}
		return
	}

	posix, ws := newSession("posix", sandrunv1.GuestOs_GUEST_OS_LINUX)
	out, err := sync(posix)
	assert(err == nil, "POSIX sync: %v: %s", err, out)
	target := must(os.Readlink(filepath.Join(ws, "link")))
	assert(target == malformedTarget, "guest target=%x, want %x", target, malformedTarget)
	var got manifest
	check(json.Unmarshal(must(os.ReadFile(ws+manifestSuffix)), &got))
	assert(got.Files["link"] == entry(malformedTarget), "manifest records unexpected link entry: %+v", got.Files["link"])
	log.Printf("Linux guest: tar restored raw target=%x, manifest SHA-256=%s", target, got.Files["link"].Hash)
}
