package ownsand

import (
	"bytes"
	"context"
	"testing"

	"github.com/delinoio/mono/packages/ownsand-sdk-go/ownsandtest"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestBasicOperationsOnBothTransports(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode TransportMode
		want ownsandv1.DataPathTransport
	}{
		{"relay", TransportRelay, ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_RELAY},
		{"peer-to-peer", TransportPeerToPeer, ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_PEER_TO_PEER},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.mode)
			e.program("mixed", func(ctx context.Context, x *ownsandtest.ExecIO) {
				x.Stdout([]byte("out"))
				x.Stderr([]byte("err"))
				x.Exit(3)
			})
			res, evs, out, errOut, err := e.run(ExecOptions{Command: []string{"mixed"}})
			if err != nil {
				t.Fatal(err)
			}
			if out != "out" || errOut != "err" || res.Outcome != ExecExited || *res.ExitCode != 3 || res.Transport != tc.want {
				t.Fatalf("result %+v out %q err %q", res, out, errOut)
			}
			if count(evs, ExecEventStarted) != 1 || evs[len(evs)-1].Kind != ExecEventExited {
				t.Fatalf("events %v", kinds(evs))
			}

			data := bytes.Repeat([]byte("0123456789abcdef"), 200_000) // 3.2 MB, several chunks
			n, err := e.sb.WriteFile(e.ctx, "/work/data.bin", bytes.NewReader(data), 0o640)
			if err != nil || n != int64(len(data)) {
				t.Fatalf("write: %d %v", n, err)
			}
			got, ok := e.srv.Agent.File("/work/data.bin")
			if !ok || !bytes.Equal(got, data) {
				t.Fatal("written file differs")
			}
			back, err := e.sb.ReadFileBytes(e.ctx, "/work/data.bin")
			if err != nil || !bytes.Equal(back, data) {
				t.Fatalf("read back: %v", err)
			}
			if err := e.sb.WriteFileBytes(e.ctx, "/work/empty", nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if b, ok := e.srv.Agent.File("/work/empty"); !ok || len(b) != 0 {
				t.Fatal("empty file not written")
			}
			if e.sb.Transport() != tc.want {
				t.Fatalf("transport %v", e.sb.Transport())
			}
		})
	}
}
