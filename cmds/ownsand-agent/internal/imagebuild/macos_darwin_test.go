// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo && arm64

package imagebuild

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/Code-Hex/vz/v3"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime/vzrt"
	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

const fixtureHelperVersion = "first-boot-fixture"

type firstBootGuest struct {
	ownsandv1connect.UnimplementedGuestServiceHandler
	commands chan []string
	shutdown func()
}

func (*firstBootGuest) CheckReady(context.Context, *connect.Request[ownsandv1.CheckReadyRequest]) (*connect.Response[ownsandv1.CheckReadyResponse], error) {
	return connect.NewResponse(&ownsandv1.CheckReadyResponse{HelperVersion: fixtureHelperVersion}), nil
}

func (g *firstBootGuest) Serve(_ context.Context, stream *connect.BidiStream[ownsandv1.ServeRequest, ownsandv1.ServeResponse]) error {
	req, err := stream.Receive()
	if err != nil {
		return err
	}
	frame := req.GetFrame()
	command := frame.GetExecRequest().GetCommand()
	g.commands <- command
	if slices.Equal(command, []string{"/sbin/shutdown", "-h", "now"}) {
		g.shutdown()
		panic(http.ErrAbortHandler) // The guest disconnects without a shutdown reply.
	}
	exited := &ownsandv1.ExecExited{Status: &ownsandv1.ExecExited_ExitCode{ExitCode: 0}}
	event := &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Exited{Exited: exited}}
	return stream.Send(&ownsandv1.ServeResponse{Frame: datapath.MustNewFrame(frame.GetStreamId(), frame.GetExecRequest().GetSandboxId(), event)})
}

func TestMacOSImageFirstBootShutdown(t *testing.T) {
	const guestOS = ownsandv1.GuestOs_GUEST_OS_MACOS
	shutdownCommand := []string{"/sbin/shutdown", "-h", "now"}
	tests := []struct {
		name   string
		states []vz.VirtualMachineState
		cancel bool
		err    string
	}{
		{name: "stopped", states: []vz.VirtualMachineState{vz.VirtualMachineStateStopped}},
		{name: "VM error", states: []vz.VirtualMachineState{vz.VirtualMachineStateError}, err: "the first-boot VM failed during shutdown (VirtualMachineStateError)"},
		{name: "timeout", states: []vz.VirtualMachineState{vz.VirtualMachineStateStopping}, err: "the guest did not shut down"},
		{name: "canceled", cancel: true, err: context.Canceled.Error()},
		{name: "canceled with stopped state", states: []vz.VirtualMachineState{vz.VirtualMachineStateStopped}, cancel: true, err: context.Canceled.Error()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), images.MacOSDir)
			build := images.BuildDir(dir)
			writeMacOSFixture(t, dir, "previous")
			writeMacOSFixture(t, build, "staged")
			if err := images.WriteMetadata(dir, images.Metadata{GuestOS: guestOS, GuestVersion: "previous"}); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			states := make(chan vz.VirtualMachineState, 1)
			exit := vzrt.WatchVMExit(states)
			t.Cleanup(func() { close(states) })
			guest := &firstBootGuest{
				commands: make(chan []string, len(macOSProvisionCommands)+1),
				shutdown: func() {
					if tt.cancel {
						cancel()
					}
					for _, state := range tt.states {
						states <- state
					}
				},
			}
			mux := http.NewServeMux()
			mux.Handle(ownsandv1connect.NewGuestServiceHandler(guest))
			srv := httptest.NewUnstartedServer(mux)
			srv.Config.Protocols = new(http.Protocols)
			srv.Config.Protocols.SetUnencryptedHTTP2(true)
			srv.Start()
			t.Cleanup(srv.Close)
			dial := func(ctx context.Context) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "tcp", srv.Listener.Addr().String())
			}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			err := finishMacOSImage(t.Context(), dir, images.Metadata{GuestOS: guestOS}, "", func() (string, error) {
				return provisionAndShutdownMacOS(ctx, dial, exit, logger, time.Now(), 100*time.Millisecond)
			})
			for _, command := range append(slices.Clone(macOSProvisionCommands), shutdownCommand) {
				select {
				case got := <-guest.commands:
					if !slices.Equal(got, command) {
						t.Fatalf("guest command = %q, want %q", got, command)
					}
				default:
					t.Fatalf("guest did not receive %q (build error: %v)", command, err)
				}
			}
			if tt.err == "" {
				if err != nil {
					t.Fatal(err)
				}
				assertMacOSFixture(t, dir, fixtureHelperVersion, "staged")
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Fatalf("build error = %v, want %q", err, tt.err)
			}
			if tt.cancel && !errors.Is(err, context.Canceled) {
				t.Fatalf("build error = %v, want context.Canceled", err)
			}
			if _, err := os.Stat(filepath.Join(build, images.MetadataFile)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("staged metadata stat error = %v, want os.ErrNotExist", err)
			}
			assertMacOSFixture(t, dir, "previous", "previous")
		})
	}
}

func writeMacOSFixture(t *testing.T, dir, contents string) {
	t.Helper()
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{images.MacOSDisk, images.MacOSAuxStorage, images.MacOSHardwareModel} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func assertMacOSFixture(t *testing.T, dir, helperVersion, diskContents string) {
	t.Helper()
	metadata, err := images.ReadMetadata(dir)
	if err != nil || metadata.GuestVersion != helperVersion {
		t.Fatalf("installed helper version = %q, %v, want %q", metadata.GuestVersion, err, helperVersion)
	}
	disk, err := os.ReadFile(filepath.Join(dir, images.MacOSDisk))
	if err != nil || string(disk) != diskContents {
		t.Fatalf("installed disk = %q, %v, want %q", disk, err, diskContents)
	}
}
