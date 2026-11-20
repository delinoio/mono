package guestconn

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-guest/guest"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func newClient(t *testing.T) *Client {
	t.Helper()
	if goruntime.GOOS == "windows" {
		t.Skip("uses POSIX commands")
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := guest.NewServer(guest.Config{StateDir: t.TempDir(), Logger: log, SetClock: func(time.Time) error { return nil }})
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.ServeConn(ctx, a) }()
	c := New(b, log)
	t.Cleanup(func() {
		_ = c.Close()
		cancel()
	})
	return c
}

func TestReadyExecAndLargeRead(t *testing.T) {
	c := newClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := c.Open(datapath.NewFrame(0, "sb", func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_ExecRequest{ExecRequest: &ownsandv1.ExecRequest{
			SandboxId: "sb", ExecId: datapath.NewID(), Command: []string{"/bin/sh", "-c", "echo hi"}, BindingSalt: datapath.NewBindingSalt(),
		}}
	}))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	for {
		f, err := s.Recv(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if o := f.GetExecEvent().GetOutput(); o != nil {
			out.Write(o.GetData())
		}
		if f.GetExecEvent().GetExited() != nil {
			break
		}
	}
	s.Close()
	if out.String() != "hi\n" {
		t.Fatalf("output %q", out.String())
	}

	// A read larger than the guest's initial window completes because Recv
	// returns credit.
	path := filepath.Join(t.TempDir(), "big")
	content := bytes.Repeat([]byte{7}, 3*guest.InitialWindow)
	_ = os.WriteFile(path, content, 0o644)
	s, err = c.Open(datapath.NewFrame(0, "sb", func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_FileReadRequest{FileReadRequest: &ownsandv1.ReadFileRequest{
			ReadId: datapath.NewID(), Path: path, BindingSalt: datapath.NewBindingSalt(),
		}}
	}))
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	for {
		f, err := s.Recv(ctx)
		if err != nil {
			t.Fatal(err)
		}
		n += int64(len(f.GetFileChunk().GetData()))
		if f.GetFileReadFinished() != nil {
			break
		}
	}
	if n != int64(len(content)) {
		t.Fatalf("read %d bytes", n)
	}
}

func TestConnectionCloseFailsStreams(t *testing.T) {
	c := newClient(t)
	s, err := c.Open(datapath.NewFrame(0, "sb", func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_ExecRequest{ExecRequest: &ownsandv1.ExecRequest{
			ExecId: datapath.NewID(), Command: []string{"/bin/sh", "-c", "sleep 5"}, BindingSalt: datapath.NewBindingSalt(),
		}}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recv(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if _, err := s.Recv(context.Background()); err == nil {
		t.Fatal("stream still open after the connection closed")
	}
}
