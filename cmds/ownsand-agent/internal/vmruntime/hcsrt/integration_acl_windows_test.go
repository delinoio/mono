// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt_test

import (
	"path/filepath"
	"runtime"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmacl"
)

type vmACE struct {
	sid   string
	kind  vmacl.Principal
	mask  windows.ACCESS_MASK
	flags uint8
}

func vmACEs(t *testing.T, path string) []vmACE {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.KeepAlive(sd)
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	var entries []vmACE
	for i := range uint32(acl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if kind := vmacl.Classify(sid); kind != vmacl.Other {
			entries = append(entries, vmACE{sid: sid.String(), kind: kind, mask: ace.Mask, flags: ace.Header.AceFlags})
		}
	}
	return entries
}

func checkSandboxDiskACL(t *testing.T, dir, disk string) string {
	t.Helper()
	if aces := vmACEs(t, dir); len(aces) != 0 {
		t.Fatalf("VM directory grants Hyper-V workers access: %+v", aces)
	}
	aces := vmACEs(t, filepath.Join(dir, disk))
	if len(aces) != 1 || aces[0].kind != vmacl.Worker || vmacl.ReadOnly(aces[0].mask) ||
		aces[0].flags&windows.INHERITED_ACE != 0 {
		t.Fatalf("VM disk must have one explicit per-VM write grant: %+v", aces)
	}
	return aces[0].sid
}
