// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux

package agent

import (
	"context"
	"os"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/cpclient"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/testcp"
)

// TestEnrollStopsBeforeCallWhenPendingKeyIsNotDurable checks that a state
// directory that cannot be flushed ends the enrollment before the RPC, so the
// one-time token is not spent, and that a retry with the same token enrolls
// once the directory works again.
func TestEnrollStopsBeforeCallWhenPendingKeyIsNotDurable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a directory without the read permission")
	}
	cp := testcp.New(t)
	cp.ConsumeEnrollToken()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client, err := cpclient.New(cp.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Without the read permission a directory still accepts the identity
	// file (write and search permission), but it cannot be opened to flush
	// its entries.
	if err := os.Chmod(st.Dir(), 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(st.Dir(), 0o700) })
	if _, err := Enroll(context.Background(), st, client, testcp.Token, "host", quietLogger()); err == nil {
		t.Fatal("enroll succeeded although the state directory could not be flushed")
	}
	if keys := cp.EnrollPublicKeys(); len(keys) != 0 {
		t.Fatalf("%d Enroll calls reached the control plane before the key was durable", len(keys))
	}
	if err := os.Chmod(st.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	id, err := Enroll(context.Background(), st, client, testcp.Token, "host", quietLogger())
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if id.DeviceID == "" {
		t.Fatalf("retry identity: %+v", id)
	}
	if keys := cp.EnrollPublicKeys(); len(keys) != 1 {
		t.Fatalf("%d Enroll calls reached the control plane, want 1", len(keys))
	}
}

// TestEnrollKeyIsOnDiskWhenCallArrives checks that the pending key is
// stored when the Enroll call reaches the control plane, and that a retry
// after a lost response sends the same key (the token is single-use for its
// first key).
func TestEnrollKeyIsOnDiskWhenCallArrives(t *testing.T) {
	cp := testcp.New(t)
	cp.ConsumeEnrollToken()
	cp.DropNextEnrollResponse()
	hold := cp.HoldNextEnroll()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client, err := cpclient.New(cp.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() {
		_, err := Enroll(context.Background(), st, client, testcp.Token, "host", quietLogger())
		errc <- err
	}()
	<-hold.Arrived()
	onDisk := requirePendingKey(t, st)
	hold.Release()
	if err := <-errc; err == nil {
		t.Fatal("enroll succeeded although the response was dropped")
	}
	if _, err := Enroll(context.Background(), st, client, testcp.Token, "host", quietLogger()); err != nil {
		t.Fatalf("retry: %v", err)
	}
	keys := cp.EnrollPublicKeys()
	if len(keys) != 2 || string(keys[0]) != string(onDisk) || string(keys[1]) != string(onDisk) {
		t.Fatalf("Enroll calls used keys %x, want two calls with the stored key %x", keys, onDisk)
	}
}
