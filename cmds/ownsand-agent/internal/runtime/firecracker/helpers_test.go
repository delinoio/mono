package firecracker

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/update"
)

func TestAllocatorLinks(t *testing.T) {
	a := NewAllocator(netip.MustParsePrefix("10.231.0.0/16"))
	l0, err := a.Allocate(-1)
	if err != nil {
		t.Fatal(err)
	}
	if l0.Tap != "osb0" || l0.Gateway.String() != "10.231.0.1" || l0.Guest.String() != "10.231.0.2" || l0.Subnet.String() != "10.231.0.0/30" || l0.MAC != "06:00:0a:e7:00:02" {
		t.Fatalf("link 0: %+v", l0)
	}
	l1, _ := a.Allocate(-1)
	if l1.Index != 1 || l1.Gateway.String() != "10.231.0.5" {
		t.Fatalf("link 1: %+v", l1)
	}
	if _, err := a.Allocate(1); err != ErrNoLink {
		t.Fatal("a taken preferred link was allocated")
	}
	a.Release(1)
	if l, err := a.Allocate(1); err != nil || l.Index != 1 {
		t.Fatalf("released link not reusable: %v", err)
	}
	if l := a.LinkAt(16383); l.Gateway.String() != "10.231.255.253" {
		t.Fatalf("last link %v", l.Gateway)
	}
	small := NewAllocator(netip.MustParsePrefix("192.0.2.0/29"))
	_, _ = small.Allocate(-1)
	_, _ = small.Allocate(-1)
	if _, err := small.Allocate(-1); err != ErrNoLink {
		t.Fatal("exhausted range allocated a link")
	}
}

func TestHandshake(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	go func() {
		r := bufio.NewReader(b)
		line, _ := r.ReadString('\n')
		if line != "CONNECT 1024\n" {
			_, _ = b.Write([]byte("ERR\n"))
			return
		}
		_, _ = b.Write([]byte("OK 1073741824\nguest-data"))
	}()
	if err := Handshake(a, 1024, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 10)
	if _, err := io.ReadFull(a, buf); err != nil || string(buf) != "guest-data" {
		t.Fatalf("the handshake consumed guest data: %q", buf)
	}
	c, d := net.Pipe()
	go func() {
		_, _ = bufio.NewReader(d).ReadString('\n')
		_ = d.Close()
	}()
	if err := Handshake(c, 1024, time.Second); err == nil {
		t.Fatal("a closed connection passed the handshake")
	}
}

func TestVerifyImage(t *testing.T) {
	dir := t.TempDir()
	if st, _ := VerifyImage(dir, nil, false); st != ImageMissing {
		t.Fatalf("status %v", st)
	}
	_ = os.WriteFile(filepath.Join(dir, KernelFile), []byte("kernel"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, RootfsFile), []byte("rootfs"), 0o600)
	if st, _ := VerifyImage(dir, nil, false); st != ImageInvalid {
		t.Fatalf("unsigned image status %v", st)
	}
	if st, _ := VerifyImage(dir, nil, true); st != ImageUnsigned {
		t.Fatalf("development mode status %v", st)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	keys := []update.ReleaseKey{{ID: "k", PublicKey: pub}}
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	payload, _ := json.Marshal(ImageInfo{Version: "1", KernelSHA256: sum("kernel"), RootfsSHA256: sum("rootfs")})
	signed, _ := update.SignPayload(payload, "k", priv)
	_ = os.WriteFile(filepath.Join(dir, ImageManifest), signed, 0o600)
	if st, err := VerifyImage(dir, keys, false); st != ImageVerified {
		t.Fatalf("signed image: %v %v", st, err)
	}
	_ = os.WriteFile(filepath.Join(dir, RootfsFile), []byte("tampered"), 0o600)
	if st, _ := VerifyImage(dir, keys, false); st != ImageInvalid {
		t.Fatalf("tampered image status %v", st)
	}
}

func TestAPIClient(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("unix sockets")
	}
	sock := filepath.Join(t.TempDir(), "api.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+" "+string(body))
		if r.URL.Path == "/fail" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"fault_message":"bad thing"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	api := NewAPI(sock)
	if err := api.Do(context.Background(), "PATCH", "/vm", VMState{State: "Paused"}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != `PATCH /vm {"state":"Paused"}` {
		t.Fatalf("request %v", got)
	}
	err = api.Do(context.Background(), "PUT", "/fail", nil)
	if err == nil || !strings.Contains(err.Error(), "bad thing") {
		t.Fatalf("error %v", err)
	}
}
