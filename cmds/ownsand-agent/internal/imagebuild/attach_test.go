// SPDX-License-Identifier: FSL-1.1-ALv2

package imagebuild

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

const attachPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>system-entities</key>
	<array>
		<dict>
			<key>content-hint</key>
			<string>GUID_partition_scheme</string>
			<key>dev-entry</key>
			<string>/dev/disk4</string>
			<key>potentially-mountable</key>
			<false/>
			<key>unmapped-content-hint</key>
			<string>GUID_partition_scheme</string>
		</dict>
		<dict>
			<key>content-hint</key>
			<string>C12A7328-F81F-11D2-BA4B-00A0C93EC93B</string>
			<key>dev-entry</key>
			<string>/dev/disk4s1</string>
			<key>potentially-mountable</key>
			<true/>
		</dict>
		<dict>
			<key>content-hint</key>
			<string>7C3457EF-0000-11AA-AA11-00306543ECAC</string>
			<key>dev-entry</key>
			<string>/dev/disk4s2</string>
		</dict>
	</array>
</dict>
</plist>
`

const attachPlistNoGUID = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>system-entities</key>
	<array>
		<dict>
			<key>content-hint</key>
			<string>Apple_partition_scheme</string>
			<key>dev-entry</key>
			<string>/dev/disk7</string>
		</dict>
		<dict>
			<key>content-hint</key>
			<string>Apple_HFS</string>
			<key>dev-entry</key>
			<string>/dev/disk7s2</string>
		</dict>
	</array>
</dict>
</plist>
`

func TestAttachedWholeDisk(t *testing.T) {
	for _, tc := range []struct {
		name, out, whole string
		guid             bool
	}{
		{"guid entity", attachPlist, "disk4", true},
		{"guid entity after its slices", strings.Replace(attachPlist, "/dev/disk4<", "/dev/disk4s9<", 1), "disk4", true},
		{"no guid entity", attachPlistNoGUID, "disk7", false},
		{"truncated plist", attachPlist[:strings.Index(attachPlist, "</array>")], "disk4", true},
		{"not a plist", "attached /dev/disk12s1\n", "disk12", false},
		{"no device", "<plist><dict></dict></plist>", "", false},
		{"empty", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			whole, guid := attachedWholeDisk([]byte(tc.out))
			if whole != tc.whole || guid != tc.guid {
				t.Fatalf("attachedWholeDisk = %q %v, want %q %v", whole, guid, tc.whole, tc.guid)
			}
		})
	}
}

// fakeHdiutil is a hostRunner that records every call. The attach call
// returns attachOut and attachErr; a detach call fails for each error in
// detachErrs in turn.
type fakeHdiutil struct {
	attachOut  string
	attachErr  error
	detachErrs []error
	calls      [][]string
	ctxErrs    []error // ctx.Err() of each call when the call starts
	deadlines  []time.Time
}

func (f *fakeHdiutil) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	deadline, _ := ctx.Deadline()
	f.deadlines = append(f.deadlines, deadline)
	if args[0] == "attach" {
		return []byte(f.attachOut), f.attachErr
	}
	if len(f.detachErrs) > 0 {
		err := f.detachErrs[0]
		f.detachErrs = f.detachErrs[1:]
		return nil, err
	}
	return nil, nil
}

func (f *fakeHdiutil) detaches() [][]string {
	var out [][]string
	for _, c := range f.calls {
		if c[1] == "detach" {
			out = append(out, c[1:])
		}
	}
	return out
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestWithAttachedDiskDetachesAfterUse(t *testing.T) {
	useErr := errors.New("convert the volume list: plutil exited")
	for _, tc := range []struct {
		name       string
		out        string
		use        error
		detachErrs []error
		wantErr    error
		wantDetach [][]string
		wantUse    bool
	}{
		{name: "happy path", out: attachPlist, wantUse: true, wantDetach: [][]string{{"detach", "/dev/disk4"}}},
		{name: "later step fails", out: attachPlist, use: useErr, wantErr: useErr, wantUse: true, wantDetach: [][]string{{"detach", "/dev/disk4"}}},
		{name: "no GUID entity", out: attachPlistNoGUID, wantErr: errNoGUIDPartitionTable, wantDetach: [][]string{{"detach", "/dev/disk7"}}},
		{name: "malformed plist without a GUID entity", out: "<plist><dict>/dev/disk9s1", wantErr: errNoGUIDPartitionTable, wantDetach: [][]string{{"detach", "/dev/disk9"}}},
		{
			name: "detach fails then forced detach succeeds", out: attachPlist, wantUse: true,
			detachErrs: []error{errors.New("resource busy")},
			wantDetach: [][]string{{"detach", "/dev/disk4"}, {"detach", "-force", "/dev/disk4"}},
		},
		{
			name: "forced detach fails after a successful use", out: attachPlist, wantUse: true,
			detachErrs: []error{errors.New("resource busy"), errors.New("still busy")},
			wantErr:    errors.New("still busy"),
			wantDetach: [][]string{{"detach", "/dev/disk4"}, {"detach", "-force", "/dev/disk4"}},
		},
		{
			name: "forced detach fails but the original error stays", out: attachPlist, use: useErr, wantUse: true,
			detachErrs: []error{errors.New("resource busy"), errors.New("still busy")},
			wantErr:    useErr,
			wantDetach: [][]string{{"detach", "/dev/disk4"}, {"detach", "-force", "/dev/disk4"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeHdiutil{attachOut: tc.out, detachErrs: tc.detachErrs}
			used := ""
			err := withAttachedDisk(context.Background(), f.run, "/build/disk.img", discardLogger(), func(whole string) error {
				used = whole
				return tc.use
			})
			switch {
			case tc.wantErr == nil && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != nil && (err == nil || (!errors.Is(err, tc.wantErr) && err.Error() != tc.wantErr.Error())):
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if got := f.detaches(); !reflect.DeepEqual(got, tc.wantDetach) {
				t.Fatalf("detach calls = %v, want %v", got, tc.wantDetach)
			}
			if (used != "") != tc.wantUse {
				t.Fatalf("use called with %q, want called = %v", used, tc.wantUse)
			}
			attach := f.calls[0]
			if attach[0] != hdiutilPath || attach[1] != "attach" || attach[len(attach)-1] != "/build/disk.img" {
				t.Fatalf("attach call = %v", attach)
			}
		})
	}
}

func TestWithAttachedDiskNoDetachWithoutAttach(t *testing.T) {
	attachErr := errors.New("hdiutil: attach failed")
	f := &fakeHdiutil{attachErr: attachErr}
	err := withAttachedDisk(context.Background(), f.run, "/build/disk.img", discardLogger(), func(string) error {
		t.Fatal("use called after a failed attach")
		return nil
	})
	if !errors.Is(err, attachErr) || len(f.detaches()) != 0 {
		t.Fatalf("err = %v, detaches = %v", err, f.detaches())
	}

	// An attach output without a device leaves nothing to detach by name.
	f = &fakeHdiutil{attachOut: "<plist><dict/></plist>"}
	if err := withAttachedDisk(context.Background(), f.run, "/build/disk.img", discardLogger(), func(string) error { return nil }); err == nil || len(f.detaches()) != 0 {
		t.Fatalf("err = %v, detaches = %v", err, f.detaches())
	}
}

// A canceled build context must not cancel the cleanup, and the cleanup is
// bounded.
func TestWithAttachedDiskDetachesAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeHdiutil{attachOut: attachPlist}
	err := withAttachedDisk(ctx, f.run, "/build/disk.img", discardLogger(), func(string) error {
		cancel()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the cancellation", err)
	}
	if got := f.detaches(); len(got) != 1 {
		t.Fatalf("detach calls = %v", got)
	}
	last := len(f.calls) - 1
	if f.ctxErrs[last] != nil {
		t.Fatalf("detach ran on a canceled context: %v", f.ctxErrs[last])
	}
	if d := f.deadlines[last]; d.IsZero() || time.Until(d) > detachTimeout {
		t.Fatalf("detach context deadline = %v, want one within %v", d, detachTimeout)
	}
}
