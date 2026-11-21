// SPDX-License-Identifier: FSL-1.1-ALv2

package vmruntime

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestFinishSnapshot(t *testing.T) {
	info := SnapshotInfo{SizeBytes: 42, HasMemoryState: true, MacOSHardwareModel: []byte("hw")}
	capErr := errors.New("copy failed")
	resErr := errors.New("resume rejected")

	t.Run("no errors keeps the info", func(t *testing.T) {
		got, err := FinishSnapshot(info, nil, nil)
		if err != nil || !reflect.DeepEqual(got, info) {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	t.Run("capture error without resume error is unchanged", func(t *testing.T) {
		_, err := FinishSnapshot(SnapshotInfo{}, capErr, nil)
		if err != capErr {
			t.Fatalf("got %v, want the capture error", err)
		}
	})
	t.Run("resume error fails a successful capture", func(t *testing.T) {
		got, err := FinishSnapshot(info, nil, resErr)
		if !errors.Is(err, resErr) || !strings.Contains(err.Error(), "resume after snapshot") {
			t.Fatalf("error %v, want a resume error", err)
		}
		if !reflect.DeepEqual(got, SnapshotInfo{}) {
			t.Fatalf("info %+v, want zero", got)
		}
	})
	t.Run("capture error wins and carries the resume error", func(t *testing.T) {
		_, err := FinishSnapshot(SnapshotInfo{}, capErr, resErr)
		if !errors.Is(err, capErr) {
			t.Fatalf("error %v does not wrap the capture error", err)
		}
		if errors.Is(err, resErr) {
			t.Fatalf("error %v must not match the resume error", err)
		}
		if !strings.Contains(err.Error(), "resume rejected") {
			t.Fatalf("error %q lacks the resume error text", err)
		}
	})
}
