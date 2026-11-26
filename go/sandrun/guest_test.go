// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
)

func TestAppendGuestStderr(t *testing.T) {
	const limit = 65_536
	chunks := [][]byte{
		bytes.Repeat([]byte("A"), 32<<10),
		bytes.Repeat([]byte("B"), 24<<10),
		bytes.Repeat([]byte("C"), 16<<10),
		[]byte("FINAL-TRANSFER-ERROR\n"),
		bytes.Repeat([]byte("0123456789"), 7_000),
		nil,
		[]byte("after oversized event\n"),
		bytes.Repeat([]byte("D"), limit),
	}
	var delivered, tail []byte
	for i, chunk := range chunks {
		delivered = append(delivered, chunk...)
		tail = appendGuestStderr(tail, chunk)
		want := delivered[max(len(delivered)-limit, 0):]
		if !bytes.Equal(tail, want) {
			t.Fatalf("event %d: captured %d bytes, want the last %d bytes in stream order", i, len(tail), len(want))
		}
	}
}

func TestRunGuestStderr(t *testing.T) {
	const message = "sandrun: the guest probe command exited with code 1"
	const marker = "FINAL-TRANSFER-ERROR"
	for _, tc := range []struct {
		name   string
		chunks []string
		want   string
	}{
		{
			name: "long",
			chunks: []string{
				strings.Repeat("A", 32<<10), strings.Repeat("B", 24<<10),
				strings.Repeat("C", 16<<10), marker + "\n",
			},
			want: message + ": ..." + strings.Repeat("C", 2000-len(marker)) + marker,
		},
		{
			name:   "short",
			chunks: []string{" \t short diagnostic \n"},
			want:   message + ": short diagnostic",
		},
		{name: "empty", want: message},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newExecRetryFixture(t, func(_ context.Context, _ int, send func(*sandrunv1.DevSessionExecEvent) error) error {
				if err := send(retryStarted); err != nil {
					return err
				}
				var offset int64
				for _, chunk := range tc.chunks {
					if err := send(&sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Stderr{
						Stderr: &sandrunv1.DevSessionExecOutput{Offset: offset, Data: []byte(chunk)},
					}}); err != nil {
						return err
					}
					offset += int64(len(chunk))
				}
				return send(&sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Exited{
					Exited: &sandrunv1.DevSessionExecExited{Status: &sandrunv1.DevSessionExecExited_ExitCode{ExitCode: 1}},
				}})
			})
			session := &sandrunv1.DevSession{GuestOs: sandrunv1.GuestOs_GUEST_OS_LINUX}
			r, err := c.runGuest(t.Context(), session, opProbe, nil)
			if err != nil {
				t.Fatal(err)
			}
			delivered := strings.Join(tc.chunks, "")
			want := delivered[max(len(delivered)-65_536, 0):]
			if string(r.stderr) != want {
				t.Errorf("captured %d stderr bytes, want the last %d bytes in stream order", len(r.stderr), len(want))
			}
			var failure *connect.Error
			if err := r.failure(opProbe); !errors.As(err, &failure) || failure.Code() != connect.CodeInternal || failure.Message() != tc.want {
				t.Errorf("unexpected probe failure: %v", err)
			}
		})
	}
}

func TestGuestCommandsAndPathsWorkingDirectory(t *testing.T) {
	windows := sandrunv1.GuestOs_GUEST_OS_WINDOWS
	for _, tc := range []struct {
		os        sandrunv1.GuestOs
		workspace string
		want      string
	}{
		{sandrunv1.GuestOs_GUEST_OS_LINUX, "/home/dev/work", "/"},
		{sandrunv1.GuestOs_GUEST_OS_MACOS, "/Users/dev/work", "/"},
		{windows, `C:\work`, `C:\`},
		{windows, `D:\a\b\`, `D:\`},
		{windows, `c:\work`, `c:\`},
		{windows, `C:work`, ""},
		{windows, `\\host\share\work`, ""},
		{windows, `1:\work`, ""},
		{windows, "", ""},
	} {
		if got := guestRoot(&sandrunv1.DevSession{GuestOs: tc.os, WorkspacePath: tc.workspace}); got != tc.want {
			t.Errorf("guest root for %s workspace %q = %q, want %q", tc.os, tc.workspace, got, tc.want)
		}
	}
}

func TestGuestCommandsAndPathsResolve(t *testing.T) {
	windows := &sandrunv1.DevSession{GuestOs: sandrunv1.GuestOs_GUEST_OS_WINDOWS, WorkspacePath: `C:\work`}
	linux := &sandrunv1.DevSession{GuestOs: sandrunv1.GuestOs_GUEST_OS_LINUX, WorkspacePath: "/work"}
	for _, tc := range []struct {
		name    string
		session *sandrunv1.DevSession
		path    string
		want    string
	}{
		{"UNC backslashes", windows, `\\buildhost\share\results.txt`, `\\buildhost\share\results.txt`},
		{"UNC forward slashes", windows, "//buildhost/share/results.txt", "//buildhost/share/results.txt"},
		{"UNC forward slash then backslash", windows, `/\buildhost/share/results.txt`, `/\buildhost/share/results.txt`},
		{"UNC backslash then forward slash", windows, `\/buildhost/share/results.txt`, `\/buildhost/share/results.txt`},
		{"drive root", windows, "D:/out/results.txt", "D:/out/results.txt"},
		{"Windows workspace relative", windows, "out/results.txt", `C:\work\out\results.txt`},
		{"single forward slash", windows, "/out/results.txt", `C:\work\out\results.txt`},
		{"single backslash", windows, `\out\results.txt`, `C:\work\\out\results.txt`},
		{"POSIX absolute", linux, "/var/results.txt", "/var/results.txt"},
		{"POSIX workspace relative", linux, "results.txt", "/work/results.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := guestResolve(tc.session, tc.path); got != tc.want {
				t.Errorf("guestResolve(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}
