// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/go/ownsand/datapath"
	own "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	sandrun "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/agent"
)

const (
	fixtureCredential = "sandrun-synthetic-credential-only-for-probe"
	credentialEnv     = "SANDRUN_PROBE_CREDENTIAL"
	stdoutMarker      = "runner stdout marker"
	stderrMarker      = "runner stderr marker"
)

func main() {
	if len(os.Args) == 4 && os.Args[1] == "mark" {
		if err := mark(os.Args[2], os.Args[3]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(6)
		}
		return
	}
	if len(os.Args) != 2 && len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: probe ownsand-guest [baseline]")
		os.Exit(2)
	}
	if err := run(os.Args[1], len(os.Args) == 3 && os.Args[2] == "baseline"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("CREDENTIAL_CLEANUP_PROBE_PASS")
}

func mark(marker, credential string) error {
	if err := os.WriteFile(marker, []byte("called"), 0o600); err != nil {
		return err
	}
	if _, err := os.Stat(credential); !errors.Is(err, os.ErrNotExist) {
		return errors.New("credential was still present at invocation")
	}
	if os.Getenv(credentialEnv) != fixtureCredential {
		return errors.New("runner did not receive the expected credential")
	}
	fmt.Println(stdoutMarker)
	fmt.Fprintln(os.Stderr, stderrMarker)
	return nil
}

func run(guestBinary string, baseline bool) (runErr error) {
	platform, wantOS := sandrun.GuestOs_GUEST_OS_LINUX, own.GuestOs_GUEST_OS_LINUX
	switch runtime.GOOS {
	case "darwin":
		platform, wantOS = sandrun.GuestOs_GUEST_OS_MACOS, own.GuestOs_GUEST_OS_MACOS
	case "windows":
		platform, wantOS = sandrun.GuestOs_GUEST_OS_WINDOWS, own.GuestOs_GUEST_OS_WINDOWS
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	root, err := os.MkdirTemp("", "credential probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	guest := exec.CommandContext(ctx, guestBinary, "-listen", "tcp://"+addr, "-state-dir", filepath.Join(root, "state"), "-clock", "ignore")
	var guestLogs bytes.Buffer
	guest.Stdout, guest.Stderr = &guestLogs, &guestLogs
	if err := guest.Start(); err != nil {
		return err
	}
	defer func() {
		_ = guest.Process.Kill()
		_ = guest.Wait()
		if containsText(guestLogs.Bytes(), fixtureCredential) {
			runErr = errors.Join(runErr, errors.New("guest diagnostic exposed the fixture credential"))
		}
	}()
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	transport := &http.Transport{Protocols: &protocols}
	defer transport.CloseIdleConnections()
	client := ownsandv1connect.NewGuestServiceClient(&http.Client{Transport: transport}, "http://"+addr)
	for {
		ready, err := client.CheckReady(ctx, connect.NewRequest(&own.CheckReadyRequest{}))
		if err == nil {
			if ready.Msg.GetGuestOs() != wantOS {
				return errors.New("unexpected guest platform")
			}
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("guest readiness: %w", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	fmt.Printf("OwnSand GuestService ready on %s/%s via Connect h2c\n", runtime.GOOS, runtime.GOARCH)
	for _, name := range []string{"blocked", "success"} {
		if err := probeCase(ctx, client, platform, root, executable, name, baseline); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

func probeCase(ctx context.Context, client ownsandv1connect.GuestServiceClient, platform sandrun.GuestOs, root, executable, name string, baseline bool) error {
	dir := filepath.Join(root, name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	credential := filepath.Join(dir, agent.CredentialFile)
	runnerLog := filepath.Join(dir, agent.LogFile)
	marker := filepath.Join(root, name+".called")
	if err := os.WriteFile(credential, []byte(fixtureCredential), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(runnerLog, nil, 0o600); err != nil {
		return err
	}
	blocked := name == "blocked"
	if blocked {
		release, err := blockRemoval(dir, credential)
		if err != nil {
			return err
		}
		defer release()
	}
	program := agent.Program{Dir: dir, CredentialEnv: credentialEnv, Args: []string{executable, "mark", marker, credential}}
	script := agent.StartScript(platform, program)
	if strings.Contains(script, fixtureCredential) {
		return errors.New("exec command contains the credential")
	}
	req := &own.ExecRequest{SandboxId: "credential-cleanup-probe", ExecId: name, Command: agent.Command(platform, script)}
	if runtime.GOOS == "windows" {
		req.Env = map[string]string{"USERPROFILE": root}
	} else {
		req.Env = map[string]string{"HOME": root, "PATH": "/usr/bin:/bin"}
	}
	code, output, err := runGuestExec(ctx, client, req)
	if err != nil {
		return err
	}
	if containsText(output, fixtureCredential) {
		return errors.New("exec stream contains the credential")
	}
	log, err := os.ReadFile(runnerLog)
	if err != nil {
		return err
	}
	if containsText(log, fixtureCredential) {
		return errors.New("runner log contains the credential")
	}
	_, markerErr := os.Stat(marker)
	if markerErr != nil && !errors.Is(markerErr, os.ErrNotExist) {
		return markerErr
	}
	_, credentialErr := os.Stat(credential)
	if credentialErr != nil && !errors.Is(credentialErr, os.ErrNotExist) {
		return credentialErr
	}
	called, retained := markerErr == nil, credentialErr == nil
	redirected := containsText(log, stdoutMarker) && containsText(log, stderrMarker)
	fmt.Printf("%s exit=%d marker=%t credential-file=%t exec-output=%d runner-output=%d redirected=%t credential-in-diagnostics=false\n",
		name, code, called, retained, len(output), len(log), redirected)
	if blocked && baseline {
		if !called || !retained || code != 6 || len(output) == 0 {
			return errors.New("baseline did not launch after a failed removal")
		}
	} else if blocked {
		if called || !retained || code != 22 || len(output) != 0 || len(log) != 0 {
			return errors.New("failed removal did not stop the runner with a silent exit 22")
		}
	} else if !called || retained || code != 0 || len(output) != 0 || !redirected {
		return errors.New("successful cleanup did not hand off the credential and redirect output")
	}
	return nil
}

// All probe strings are ASCII. Windows PowerShell can write UTF-16LE logs.
func containsText(data []byte, text string) bool {
	if bytes.Contains(data, []byte(text)) {
		return true
	}
	utf16LE := make([]byte, 2*len(text))
	for i := 0; i < len(text); i++ {
		utf16LE[2*i] = text[i]
	}
	return bytes.Contains(data, utf16LE)
}

func blockRemoval(dir, credential string) (func(), error) {
	switch runtime.GOOS {
	case "windows":
		// Go opens the file without FILE_SHARE_DELETE but still permits reads.
		f, err := os.Open(credential)
		if err != nil {
			return nil, err
		}
		return func() { _ = f.Close() }, nil
	case "linux":
		if os.Geteuid() == 0 {
			// Linux refuses to unlink a mount point, even for root.
			if err := exec.Command("/bin/busybox", "mount", "-o", "bind", credential, credential).Run(); err != nil {
				return nil, err
			}
			return func() { _ = exec.Command("/bin/busybox", "umount", credential).Run() }, nil
		}
	}
	if os.Geteuid() == 0 {
		return nil, errors.New("run the macOS probe as an unprivileged user")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		return nil, err
	}
	return func() { _ = os.Chmod(dir, 0o700) }, nil
}

func runGuestExec(ctx context.Context, client ownsandv1connect.GuestServiceClient, req *own.ExecRequest) (int64, []byte, error) {
	stream := client.Serve(ctx)
	defer stream.CloseResponse()
	if err := stream.Send(&own.ServeRequest{Frame: datapath.MustNewFrame(1, req.SandboxId, req)}); err != nil {
		return 0, nil, err
	}
	if err := stream.CloseRequest(); err != nil {
		return 0, nil, err
	}
	var output []byte
	var exit *own.ExecExited
	for {
		res, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, nil, err
		}
		ev := res.GetFrame().GetExecEvent()
		if ev.GetFailed() != nil {
			return 0, nil, fmt.Errorf("guest exec failed: %v", ev.GetFailed().GetReason())
		}
		if ev.GetExited() != nil {
			exit = ev.GetExited()
		}
		output = append(output, ev.GetStdout().GetData()...)
		output = append(output, ev.GetStderr().GetData()...)
	}
	if exit == nil {
		return 0, nil, errors.New("guest stream did not return an exec exit")
	}
	return exit.GetExitCode(), output, nil
}
