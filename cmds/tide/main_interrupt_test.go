// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/tide"
	"github.com/delinoio/mono/go/tide/tidetest"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
)

// Run main in a subprocess so assertions can use the actual exit status.
const runTideMain = "TIDE_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(runTideMain) == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

func tideCommand(t *testing.T, serverURL string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(),
		runTideMain+"=1",
		"TIDE_API_URL="+serverURL,
		"TIDE_API_KEY="+tidetest.DefaultAPIKey,
		"TIDE_CONFIG_DIR="+t.TempDir(),
		"NO_COLOR=1",
	)
	return cmd
}

func startTide(t *testing.T, cmd *exec.Cmd) <-chan struct{} {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start tide: %v", err)
	}
	ended := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(ended)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-ended
	})
	return ended
}

// The FIFO opened by os.ReadFile can use Go's poller. Inherited stdin is
// blocking, so a thread must be in read(0, ...) while it waits for data.
func waitingForCredential(pid int, fifo string) bool {
	proc := filepath.Join("/proc", strconv.Itoa(pid))
	if fifo != "" {
		fds, _ := filepath.Glob(filepath.Join(proc, "fd", "*"))
		for _, fd := range fds {
			if target, err := os.Readlink(fd); err == nil && target == fifo {
				return true
			}
		}
		return false
	}
	threads, _ := filepath.Glob(filepath.Join(proc, "task", "*", "syscall"))
	read := strconv.Itoa(syscall.SYS_READ)
	for _, thread := range threads {
		inCall, err := os.ReadFile(thread)
		if err != nil {
			continue
		}
		fields := strings.Fields(string(inCall))
		if len(fields) >= 2 && fields[0] == read && fields[1] == "0x0" {
			return true
		}
	}
	return false
}

func TestSecondInterruptEndsBlockedCredentialRead(t *testing.T) {
	for _, input := range []string{"fifo", "stdin"} {
		t.Run(input, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusBadRequest)
			}))
			srv.Config.Protocols = new(http.Protocols)
			srv.Config.Protocols.SetHTTP1(true)
			srv.Config.Protocols.SetUnencryptedHTTP2(true)
			srv.Start()
			t.Cleanup(srv.Close)
			args := []string{"--org", "tide-org", "ownsand", "link", "--ownsand-org", "ownsand-org"}
			var fifo string
			var stdin *os.File
			switch input {
			case "fifo":
				// Match the resolved path that /proc reports for the open file.
				dir, err := filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				fifo = filepath.Join(dir, "key.fifo")
				if err := syscall.Mkfifo(fifo, 0o600); err != nil {
					t.Fatal(err)
				}
				// Keep a writer open without sending data or EOF.
				hold, err := os.OpenFile(fifo, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = hold.Close() })
				args = append(args, "--api-key-file", fifo)
			case "stdin":
				r, w, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					_ = w.Close()
					_ = r.Close()
				})
				stdin = r
			}
			cmd := tideCommand(t, srv.URL, args...)
			if stdin != nil {
				cmd.Stdin = stdin
			}
			ended := startTide(t, cmd)

			deadline := time.After(10 * time.Second)
			tick := time.NewTicker(20 * time.Millisecond)
			defer tick.Stop()
			for !waitingForCredential(cmd.Process.Pid, fifo) {
				select {
				case <-ended:
					t.Fatalf("tide exited before opening credential input: %v", cmd.ProcessState)
				case <-deadline:
					t.Fatal("tide did not reach the credential read")
				case <-tick.C:
				}
			}

			if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
				t.Fatal(err)
			}
			select {
			case <-ended:
				t.Fatalf("the first interrupt ended the process: %v", cmd.ProcessState)
			case <-time.After(100 * time.Millisecond):
			}

			// Allow time for the signal handler to unregister before retrying.
			deadline = time.After(5 * time.Second)
			for {
				_ = cmd.Process.Signal(syscall.SIGINT)
				select {
				case <-ended:
					if status := cmd.ProcessState.Sys().(syscall.WaitStatus); status.Signal() != syscall.SIGINT {
						t.Fatalf("tide ended with %v, want SIGINT", cmd.ProcessState)
					}
					if got := calls.Load(); got != 0 {
						t.Fatalf("tide sent %d requests during credential input", got)
					}
					return
				case <-deadline:
					t.Fatal("tide did not exit after repeated interrupts")
				case <-tick.C:
				}
			}
		})
	}
}

func TestFirstInterruptPrintsExecAttachCommand(t *testing.T) {
	srv := tidetest.New()
	t.Cleanup(srv.Close)
	org := srv.AddOrganization("tide-org").GetOrganizationId()
	profile := srv.AddDevSessionProfile(org, "linux", tidev1.GuestOs_GUEST_OS_LINUX, t.TempDir())
	client, err := tide.New(tide.Options{BaseURL: srv.URL, APIKey: srv.APIKey})
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.DevSessions().CreateDevSession(t.Context(), connect.NewRequest(&tidev1.CreateDevSessionRequest{
		OrganizationId: org, RunnerProfileId: profile.GetRunnerProfileId(),
	}))
	if err != nil {
		t.Fatal(err)
	}
	session := res.Msg.GetDevSession().GetDevSessionId()
	cmd := tideCommand(t, srv.URL, "--org", org, "session", "exec", session, "--",
		"sh", "-c", "printf 'exec-ready\n'; exec sleep 60")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if err := r.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = w, &stderr
	ended := startTide(t, cmd)
	_ = w.Close()
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil || line != "exec-ready\n" {
		t.Fatalf("wait for exec output: got %q, %v", line, err)
	}

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
		if code := cmd.ProcessState.ExitCode(); code != 130 {
			t.Fatalf("tide exited with %d, want 130. Stderr: %s", code, &stderr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("tide exec did not exit after the first interrupt")
	}
	reqs := srv.ExecRequests()
	if len(reqs) != 1 {
		t.Fatalf("got %d exec requests, want 1", len(reqs))
	}
	attach := fmt.Sprintf("tide session attach %s %s ", session, reqs[0].GetExecId())
	if !strings.Contains(stderr.String(), attach) {
		t.Fatalf("stderr did not contain the attach command %q: %s", attach, &stderr)
	}
}
