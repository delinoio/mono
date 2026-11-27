// This acceptance probe uses a loopback Connect service and a real sandrun
// stdio subprocess. A second invocation reads the launched process environment.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	pb "github.com/delinoio/mono/protos/delino/sandrun/v1"
	rpc "github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

const configured = "synthetic-operator-2199"
const organization = "test-org"
const nativeMarker = "LOCAL_PROCESS_ENV_OK"
const fixtureMarker = "WINDOWS_FIXTURE_ENV_OK"

type scenario struct {
	name     string
	guestOS  pb.GuestOs
	value    string
	noAlias  bool
	rejected bool
}

type service struct {
	rpc.UnimplementedDevSessionServiceHandler
	scenario
	self             string
	lookups          atomic.Int32
	execs            atomic.Int32
	aliasesForwarded atomic.Bool
}

func (s *service) GetDevSession(_ context.Context, req *connect.Request[pb.GetDevSessionRequest]) (*connect.Response[pb.GetDevSessionResponse], error) {
	s.lookups.Add(1)
	if req.Msg.GetOrganizationId() != organization || req.Msg.GetDevSessionId() != s.name {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("unexpected lookup target"))
	}
	if s.rejected {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("fixture lookup rejected"))
	}
	return connect.NewResponse(&pb.GetDevSessionResponse{DevSession: &pb.DevSession{GuestOs: s.guestOS}}), nil
}

func (s *service) ExecDevSession(ctx context.Context, req *connect.Request[pb.ExecDevSessionRequest], stream *connect.ServerStream[pb.ExecDevSessionResponse]) error {
	s.execs.Add(1)
	if req.Msg.GetOrganizationId() != organization || req.Msg.GetDevSessionId() != s.name ||
		!slices.Equal(req.Msg.GetCommand(), []string{"probe-child", s.name}) {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("unexpected exec target or command"))
	}
	want := map[string]string{"BUILD_MODE": s.value, "KEEP": "1"}
	windows := s.guestOS == pb.GuestOs_GUEST_OS_WINDOWS
	if !windows && !s.noAlias {
		want["build_mode"] = "tool-lower"
		want["Build_Mode"] = "tool-mixed"
	}
	marker := fixtureMarker
	env := req.Msg.GetEnv()
	if windows {
		_, lower := env["build_mode"]
		_, mixed := env["Build_Mode"]
		s.aliasesForwarded.Store(lower || mixed)
	}
	if !windows || runtime.GOOS == "windows" {
		// Apply the OwnSand guest's ordering before os/exec constructs the
		// native environment. On Windows, os/exec folds duplicate keys.
		child := exec.CommandContext(ctx, s.self, "--child", s.name)
		for key, value := range env {
			child.Env = append(child.Env, key+"="+value)
		}
		slices.Sort(child.Env)
		out, err := child.Output()
		if err != nil || string(out) != nativeMarker+"\n" {
			return connect.NewError(connect.CodeFailedPrecondition, errors.New("native child rejected the request environment"))
		}
		marker = nativeMarker
	}
	if !maps.Equal(env, want) {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("exec request environment did not match expected names and values"))
	}
	for _, event := range []*pb.DevSessionExecEvent{
		{Event: &pb.DevSessionExecEvent_Started{Started: &pb.DevSessionExecStarted{}}},
		{Event: &pb.DevSessionExecEvent_Stdout{Stdout: &pb.DevSessionExecOutput{Data: []byte(marker)}}},
		{Event: &pb.DevSessionExecEvent_Exited{Exited: &pb.DevSessionExecExited{Status: &pb.DevSessionExecExited_ExitCode{ExitCode: 0}}}},
	} {
		if err := stream.Send(&pb.ExecDevSessionResponse{Event: event}); err != nil {
			return err
		}
	}
	return nil
}

func runChild(name string) {
	want := configured
	if name == "windows-empty" {
		want = ""
	}
	value, ok := os.LookupEnv("BUILD_MODE")
	if !ok || value != want || os.Getenv("KEEP") != "1" {
		os.Exit(2)
	}
	if runtime.GOOS == "windows" {
		if lower, ok := os.LookupEnv("build_mode"); !ok || lower != want {
			os.Exit(3)
		}
	} else if name == "linux-distinct" || name == "macos-distinct" {
		if os.Getenv("build_mode") != "tool-lower" || os.Getenv("Build_Mode") != "tool-mixed" {
			os.Exit(4)
		}
	}
	fmt.Println(nativeMarker)
}

func runScenario(ctx context.Context, cli, self, configDir string, tc scenario) error {
	fixture := &service{scenario: tc, self: self}
	_, handler := rpc.NewDevSessionServiceHandler(fixture)
	upstream := httptest.NewUnstartedServer(handler)
	upstream.Config.Protocols = new(http.Protocols)
	upstream.Config.Protocols.SetUnencryptedHTTP2(true)
	upstream.Start()
	defer upstream.Close()

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, cli, "mcp", "--org", organization, "--api-url", upstream.URL, "--pass-env", "BUILD_MODE")
	cmd.Env = append(os.Environ(), "BUILD_MODE="+tc.value, "SANDRUN_API_KEY=synthetic-test-key", "SANDRUN_CONFIG_DIR="+configDir, "NO_COLOR=1")
	cmd.Stderr = &stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "env-precedence-probe", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return fmt.Errorf("initialize MCP: %w", err)
	}
	toolEnv := map[string]string{"BUILD_MODE": "tool-exact", "KEEP": "1"}
	if !tc.noAlias {
		toolEnv["build_mode"] = "tool-lower"
		toolEnv["Build_Mode"] = "tool-mixed"
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "exec", Arguments: map[string]any{
		"session_id": tc.name, "command": []string{"probe-child", tc.name}, "env": toolEnv, "wait_seconds": 5,
	}})
	if err != nil {
		_ = session.Close()
		return fmt.Errorf("call exec: %w", err)
	}
	if err := session.Close(); err != nil {
		return fmt.Errorf("close MCP: %w", err)
	}
	body, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if bytes.Contains(body, []byte(configured)) || strings.Contains(stderr.String(), configured) {
		return errors.New("a configured value appeared in MCP output or logs")
	}
	var wantLookups, wantExecs int32 = 1, 1
	if tc.noAlias {
		wantLookups = 0
	}
	if tc.rejected {
		wantExecs = 0
	}
	if fixture.aliasesForwarded.Load() {
		return fmt.Errorf("a Windows exec request still carried a tool alias (session lookups=%d)", fixture.lookups.Load())
	}
	if fixture.lookups.Load() != wantLookups || fixture.execs.Load() != wantExecs {
		return fmt.Errorf("got %d lookups and %d execs, want %d lookups and %d execs", fixture.lookups.Load(), fixture.execs.Load(), wantLookups, wantExecs)
	}
	if tc.rejected {
		if !result.IsError || !bytes.Contains(body, []byte("permission_denied: fixture lookup rejected")) {
			return errors.New("expected the lookup error from MCP")
		}
		fmt.Printf("%s: permission_denied, exec requests=0\n", tc.name)
		return nil
	}
	wantMarker := nativeMarker
	if tc.guestOS == pb.GuestOs_GUEST_OS_WINDOWS && runtime.GOOS != "windows" {
		wantMarker = fixtureMarker
	}
	var output struct {
		ExitCode *int `json:"exit_code"`
		Stdout   struct {
			Text string `json:"text"`
		} `json:"stdout"`
	}
	structured, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(structured, &output); err != nil {
		return err
	}
	if result.IsError || output.ExitCode == nil || *output.ExitCode != 0 || output.Stdout.Text != wantMarker {
		return errors.New("exec did not return exit 0 and the expected process or fixture marker")
	}
	fmt.Printf("%s: %s, exit=0, session lookups=%d\n", tc.name, wantMarker, wantLookups)
	return nil
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "--child" {
		runChild(os.Args[2])
		return
	}
	if len(os.Args) != 2 {
		log.Fatal("usage: env-probe <path-to-sandrun>")
	}
	self, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	cli, err := filepath.Abs(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	configDir, err := os.MkdirTemp(filepath.Dir(self), "env-probe-config-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(configDir)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Printf("platform: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	cases := []scenario{
		{name: "windows-configured", guestOS: pb.GuestOs_GUEST_OS_WINDOWS, value: configured},
		{name: "windows-empty", guestOS: pb.GuestOs_GUEST_OS_WINDOWS},
		{name: "failed-lookup", guestOS: pb.GuestOs_GUEST_OS_WINDOWS, value: configured, rejected: true},
	}
	if runtime.GOOS != "windows" {
		cases = append(cases,
			scenario{name: "linux-distinct", guestOS: pb.GuestOs_GUEST_OS_LINUX, value: configured},
			scenario{name: "macos-distinct", guestOS: pb.GuestOs_GUEST_OS_MACOS, value: configured},
		)
	}
	cases = append(cases, scenario{name: "exact-match", guestOS: pb.GuestOs_GUEST_OS_WINDOWS, value: configured, noAlias: true})
	for _, tc := range cases {
		if err := runScenario(ctx, cli, self, configDir, tc); err != nil {
			log.Fatalf("%s: %v", tc.name, err)
		}
	}
}
