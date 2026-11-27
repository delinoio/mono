package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/go/ownsand/datapath"
	own "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	sandrun "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/servers/sandrun-api/internal/agent"
	"github.com/delinoio/mono/servers/sandrun-api/internal/agent/devin"
)

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func exists(name string) bool {
	_, err := os.Stat(name)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	check(err)
	return true
}

func run(ctx context.Context, client ownsandv1connect.GuestServiceClient, req *own.ExecRequest) int64 {
	stream := client.Serve(ctx)
	defer stream.CloseResponse()
	check(stream.Send(&own.ServeRequest{Frame: datapath.MustNewFrame(1, req.SandboxId, req)}))
	check(stream.CloseRequest())
	code := int64(-1)
	for {
		res, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return code
		}
		check(err)
		ev := res.GetFrame().GetExecEvent()
		if ev.GetExited() != nil {
			code = ev.GetExited().GetExitCode()
		}
		if ev.GetFailed() != nil {
			log.Fatalf("guest exec failed: %v", ev.GetFailed())
		}
		if stderr := ev.GetStderr(); stderr != nil {
			fmt.Printf("guest stderr: %s\n", stderr.GetData())
		}
	}
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "emit-macos" {
		fmt.Print(devin.InstallScript(sandrun.GuestOs_GUEST_OS_MACOS))
		return
	}
	ctx, stop := context.WithTimeout(context.Background(), 20*time.Second)
	defer stop()
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	tr := &http.Transport{Protocols: &protocols}
	defer tr.CloseIdleConnections()
	client := ownsandv1connect.NewGuestServiceClient(&http.Client{Transport: tr}, "http://127.0.0.1:9170")
	platform := sandrun.GuestOs_GUEST_OS_LINUX
	guestOS := own.GuestOs_GUEST_OS_LINUX
	if runtime.GOOS == "darwin" {
		platform = sandrun.GuestOs_GUEST_OS_MACOS
		guestOS = own.GuestOs_GUEST_OS_MACOS
	}
	for {
		ready, err := client.CheckReady(ctx, connect.NewRequest(&own.CheckReadyRequest{}))
		if err == nil {
			if ready.Msg.GetGuestOs() != guestOS {
				log.Fatal("unexpected guest platform")
			}
			break
		}
		select {
		case <-ctx.Done():
			log.Fatal(err)
		case <-time.After(25 * time.Millisecond):
		}
	}
	fmt.Printf("OwnSand GuestService ready on %s/%s via Connect h2c\n", runtime.GOOS, runtime.GOARCH)
	probeRoot := os.Getenv("PROBE_ROOT")
	if probeRoot == "" {
		probeRoot = "/"
	}
	baseline := exists(filepath.Join(probeRoot, "baseline"))
	for _, tc := range []struct {
		name, body         string
		curlCode, code     int64
		bash, preinstalled bool
	}{
		{name: "empty", curlCode: 22, code: 12},
		{name: "partial", body: `: > "$HOME/payload.called"; exit 0`, curlCode: 18, code: 12},
		{name: "bash failure", body: `: > "$HOME/payload.called"; exit 5`, code: 12, bash: true},
		{name: "no CLI", body: `: > "$HOME/payload.called"; exit 0`, code: 13, bash: true},
		{name: "installs CLI", body: `: > "$HOME/payload.called"
printf '#!/bin/sh\nexit 0\n' > "$HOME/.devin/bin/devin"
chmod +x "$HOME/.devin/bin/devin"`, bash: true},
		{name: "preinstalled", preinstalled: true},
	} {
		home := filepath.Join(probeRoot, "fixtures", tc.name)
		bin := filepath.Join(home, ".devin", "bin")
		tmp := filepath.Join(home, "installer tmp")
		check(os.MkdirAll(bin, 0755))
		check(os.MkdirAll(tmp, 0700))
		stub := func(name, contents string) {
			check(os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+contents+"\n"), 0755))
		}
		stub("git", "exit 0")
		stub("curl", `: > "$HOME/curl.called"
if [ "${3:-}" = "-o" ]; then
  printf '%s\n' "$4" > "$HOME/download-path"
  printf '%s\n' "$BODY" > "$4"
else
  printf '%s\n' "$BODY"
fi
exit "$CURL_CODE"`)
		stub("bash", `: > "$HOME/bash.called"
exec /bin/bash "$@"`)
		if tc.preinstalled {
			stub("devin", "exit 0")
		}
		code := run(ctx, client, &own.ExecRequest{
			SandboxId: "installer-probe",
			ExecId:    "installer-" + tc.name,
			Command:   agent.Command(platform, devin.InstallScript(platform)),
			Env: map[string]string{
				"HOME": home, "TMPDIR": tmp, "PATH": "/bin:/usr/bin",
				"CURL_CODE": strconv.FormatInt(tc.curlCode, 10), "BODY": tc.body,
			},
		})
		curl, bash, payload := exists(filepath.Join(home, "curl.called")), exists(filepath.Join(home, "bash.called")), exists(filepath.Join(home, "payload.called"))
		files, err := os.ReadDir(tmp)
		check(err)
		cleaned := len(files) == 0
		if !baseline && curl {
			file, err := os.ReadFile(filepath.Join(home, "download-path"))
			check(err)
			cleaned = cleaned && !exists(string(file[:len(file)-1]))
		}
		fmt.Printf("%s exit=%d curl=%t bash=%t payload=%t temp-clean=%t\n", tc.name, code, curl, bash, payload, cleaned)
		wantedCode, wantedBash, wantedPayload := tc.code, tc.bash, tc.bash
		if baseline && tc.curlCode != 0 {
			wantedCode, wantedBash, wantedPayload = 13, true, tc.body != ""
		}
		if code != wantedCode || curl == tc.preinstalled || bash != wantedBash || payload != wantedPayload || !cleaned {
			log.Fatalf("%s had unexpected result", tc.name)
		}
	}
	fmt.Println("INSTALLER_PROBE_PASS")
}
