// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-guest/guest"
	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

const sandboxID = "0192f4b6-7c3a-7def-8abc-0123456789ab"

func require(ok bool, format string, args ...any) {
	if !ok {
		panic(fmt.Sprintf(format, args...))
	}
}

func check(err error) {
	require(err == nil, "%v", err)
}

func start(cfg guest.Config) (*guest.Server, ownsandv1connect.GuestServiceClient, func()) {
	srv, err := guest.New(cfg)
	check(err)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	check(err)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- srv.Run(ctx, l) }()
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	client := ownsandv1connect.NewGuestServiceClient(
		&http.Client{Transport: &http.Transport{Protocols: &protocols}},
		"http://"+l.Addr().String(),
	)
	return srv, client, func() {
		cancel()
		check(<-stopped)
		check(srv.Close())
	}
}

func write(client ownsandv1connect.GuestServiceClient, req *ownsandv1.WriteFileRequest) *ownsandv1.DataFrame {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := client.Serve(ctx)
	f := datapath.MustNewFrame(1, sandboxID, req)
	check(stream.Send(&ownsandv1.ServeRequest{Frame: f}))
	resp, err := stream.Receive()
	check(err)
	check(stream.CloseRequest())
	return resp.GetFrame()
}

func countLinuxDescriptors(path string) int {
	entries, err := os.ReadDir("/proc/self/fd")
	check(err)
	count := 0
	for _, entry := range entries {
		target, _ := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if target == path || target == path+" (deleted)" {
			count++
		}
	}
	return count
}

func requirePaths(paths []string, present bool) {
	for _, path := range paths {
		_, err := os.Stat(path)
		if present {
			check(err)
		} else {
			require(errors.Is(err, os.ErrNotExist), "path still present after sweep: %s (%v)", filepath.Base(path), err)
		}
	}
}

func main() {
	root, err := os.MkdirTemp("", "guest-handoff-")
	check(err)
	defer os.RemoveAll(root)
	var elapsed atomic.Int64
	epoch := time.Now()
	cfg := guest.Config{
		StateDir:      filepath.Join(root, "state"),
		Clock:         guest.ClockModeIgnore,
		SweepInterval: time.Hour,
		Now:           func() time.Time { return epoch.Add(time.Duration(elapsed.Load())) },
	}
	outgoing, oldClient, stop := start(cfg)
	partial := &ownsandv1.WriteFileRequest{
		SandboxId: sandboxID, WriteId: "0192f4b6-7c3a-7def-8abc-0123456789aa",
		Path: filepath.Join(root, "partial"), BindingSalt: make([]byte, 32), Data: []byte("partial"),
	}
	published := &ownsandv1.WriteFileRequest{
		SandboxId: sandboxID, WriteId: "0192f4b6-7c3a-7def-8abc-0123456789ac",
		Path: filepath.Join(root, "published"), BindingSalt: make([]byte, 32), Data: []byte("published"),
		Finish: &ownsandv1.WriteFileFinish{SizeBytes: 9},
	}
	resp := write(oldClient, partial)
	require(resp.GetFileWriteResult().GetLength() == 7 && !resp.GetFileWriteResult().GetFinished(), "partial chunk failed: %v", resp.GetError())
	resp = write(oldClient, published)
	require(resp.GetFileWriteResult().GetFinished(), "published control failed: %v", resp.GetError())
	stages, err := filepath.Glob(filepath.Join(root, ".ownsand-*.tmp"))
	check(err)
	records, err := filepath.Glob(filepath.Join(cfg.StateDir, "writes", "*.json"))
	check(err)
	require(len(stages) == 1 && len(records) == 1, "expected one staging file and one record, got %d and %d", len(stages), len(records))
	if runtime.GOOS == "linux" {
		require(countLinuxDescriptors(stages[0]) == 1, "expected one live Linux staging descriptor")
		fmt.Println("Outgoing server holds one staging descriptor before shutdown")
	}
	requirePaths([]string{stages[0], records[0]}, true)
	_, err = guest.New(cfg)
	require(errors.Is(err, guest.ErrStateDirInUse), "simultaneous replacement acquired state: %v", err)

	stop()
	if runtime.GOOS == "linux" {
		require(countLinuxDescriptors(stages[0]) == 0, "outgoing Linux descriptor remained open at handoff")
		fmt.Println("Outgoing server holds zero staging descriptors before replacement")
	}
	replacement, client, stopReplacement := start(cfg)
	defer stopReplacement()
	requirePaths([]string{stages[0], records[0]}, true)
	detail := write(client, partial).GetError()
	require(detail.GetCode() == ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND && strings.Contains(detail.GetMessage(), "helper restart"), "recovered continuation: %v", detail)
	replacement.Sweep()
	requirePaths([]string{stages[0], records[0]}, true)
	fmt.Println("Replacement found the staging file and record and rejected continuation before expiry")

	elapsed.Store(int64(guest.OperationRetention + time.Second))
	outgoing.Sweep()
	requirePaths([]string{stages[0], records[0]}, true)
	replacement.Sweep()
	requirePaths([]string{stages[0], records[0]}, false)
	bytes, err := os.ReadFile(published.GetPath())
	check(err)
	require(string(bytes) == "published", "published control changed")
	fmt.Println("Replacement sweep removed both expired paths and preserved the published bytes")
}
