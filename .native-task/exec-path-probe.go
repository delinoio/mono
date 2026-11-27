package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/go/sandrun"
	"github.com/delinoio/mono/go/sandrun/sandruntest"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
)

const tool = "sandrun-local-path-probe"
const marker = "--report-exec-path"

type observation struct {
	Image   string
	Argv0   string
	Path    string
	PathExt string
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == marker {
		if err := report(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := probe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func report() error {
	path, err := os.Executable()
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(observation{path, os.Args[0], os.Getenv("PATH"), os.Getenv("PATHEXT")})
}

func probe() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	image, err := os.ReadFile(self)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	root, err := os.MkdirTemp("", "sandrun-local-path-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	if err := os.Chdir(root); err != nil {
		return err
	}
	defer os.Chdir(cwd)

	a, b, inherited := filepath.Join(root, "A"), filepath.Join(root, "B"), filepath.Join(root, "inherited")
	suffix, hostPathExt, guestOS := "", "", sandrunv1.GuestOs_GUEST_OS_LINUX
	if runtime.GOOS == "windows" {
		suffix, hostPathExt, guestOS = ".exe", ".EXE", sandrunv1.GuestOs_GUEST_OS_WINDOWS
	}
	for _, dir := range []string{a, b, inherited} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, tool+suffix), image, 0o755); err != nil {
			return err
		}
	}
	if runtime.GOOS == "windows" {
		if err := os.WriteFile(filepath.Join(b, tool+".com"), image, 0o755); err != nil {
			return err
		}
	}
	if err := os.Setenv("PATH", inherited); err != nil {
		return err
	}
	if err := os.Setenv("PATHEXT", hostPathExt); err != nil {
		return err
	}
	fixture := sandruntest.New()
	defer fixture.Close()
	org := fixture.AddOrganization("Local command lookup").GetOrganizationId()
	profile := fixture.AddDevSessionProfile(org, "Local probe", guestOS, root)
	client, err := sandrun.New(sandrun.Options{
		BaseURL: fixture.URL,
		TokenSource: func(context.Context) (string, error) {
			return sandruntest.DefaultAccessToken, nil
		},
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	session, err := client.DevSessions().CreateDevSession(ctx, connect.NewRequest(&sandrunv1.CreateDevSessionRequest{
		OrganizationId: org, RunnerProfileId: profile.GetRunnerProfileId(),
	}))
	if err != nil {
		return err
	}

	type scenario struct {
		name, program string
		env           map[string]string
		want          observation
	}
	scenarios := []scenario{
		{"request PATH", tool, map[string]string{"PATH": a}, observation{filepath.Join(a, tool+suffix), tool, a, hostPathExt}},
		{"omitted PATH", tool, nil, observation{filepath.Join(inherited, tool+suffix), tool, inherited, hostPathExt}},
		{"empty PATH", tool, map[string]string{"PATH": ""}, observation{}},
		{"explicit path", filepath.Join(b, tool+suffix), map[string]string{"PATH": a}, observation{filepath.Join(b, tool+suffix), filepath.Join(b, tool+suffix), a, hostPathExt}},
	}
	if runtime.GOOS == "windows" {
		for i := 1; i <= 5; i++ {
			scenarios = append(scenarios,
				scenario{fmt.Sprintf("Windows PATH aliases #%d", i), tool, map[string]string{"PATH": a, "Path": b, "PATHEXT": ".EXE"}, observation{filepath.Join(b, tool+suffix), tool, b, ".EXE"}},
				scenario{fmt.Sprintf("Windows empty winning PATH #%d", i), tool, map[string]string{"PATH": a, "Path": "", "PATHEXT": ".EXE"}, observation{}},
				scenario{fmt.Sprintf("Windows PATHEXT aliases #%d", i), tool, map[string]string{"PATH": b, "PATHEXT": ".EXE;.COM", "PathExt": ".COM;.EXE"}, observation{filepath.Join(b, tool+".com"), tool, b, ".COM;.EXE"}},
			)
		}
	} else {
		scenarios = append(scenarios,
			scenario{"POSIX case-sensitive PATH", tool, map[string]string{"PATH": a, "Path": b}, observation{filepath.Join(a, tool), tool, a, ""}},
			scenario{"POSIX lowercase alias alone", tool, map[string]string{"Path": a}, observation{filepath.Join(inherited, tool), tool, inherited, ""}},
		)
	}
	for _, s := range scenarios {
		var stdout strings.Builder
		result, err := client.Exec(ctx, sandrun.ExecParams{
			OrganizationID: org, DevSessionID: session.Msg.GetDevSession().GetDevSessionId(),
			Command: []string{s.program, marker}, Env: s.env, Timeout: 5 * time.Second,
			OnEvent: func(event sandrun.ExecEvent) error {
				if event.Kind == sandrun.ExecEventStdout {
					stdout.Write(event.Data)
				}
				return nil
			},
		})
		if err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}
		if s.want.Image == "" {
			if result.Kind != sandrun.ExecResultFailed || result.FailureReason != sandrunv1.ExecFailureReason_EXEC_FAILURE_REASON_PROGRAM_NOT_FOUND {
				return fmt.Errorf("%s: got %+v, want program not found", s.name, result)
			}
			fmt.Printf("%s: PROGRAM_NOT_FOUND\n", s.name)
			continue
		}
		var got observation
		if result.Kind != sandrun.ExecResultExited || result.ExitCode == nil || *result.ExitCode != 0 {
			return fmt.Errorf("%s: got %+v, want exit 0", s.name, result)
		}
		if err := json.Unmarshal([]byte(stdout.String()), &got); err != nil {
			return fmt.Errorf("%s: decode child output: %w", s.name, err)
		}
		gotFile, err := os.Stat(got.Image)
		if err != nil {
			return err
		}
		wantFile, err := os.Stat(s.want.Image)
		if err != nil {
			return err
		}
		if !os.SameFile(gotFile, wantFile) || got.Argv0 != s.want.Argv0 || got.Path != s.want.Path || got.PathExt != s.want.PathExt {
			return fmt.Errorf("%s: got %+v, want %+v", s.name, got, s.want)
		}
		fmt.Printf("%s: image=%s argv[0]=%q PATH=%q PATHEXT=%q exit=0\n", s.name, filepath.Base(got.Image), filepath.Base(got.Argv0), filepath.Base(got.Path), got.PathExt)
	}
	return nil
}
