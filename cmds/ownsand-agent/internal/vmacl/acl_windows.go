// SPDX-License-Identifier: FSL-1.1-ALv2

// Package vmacl handles access entries for Hyper-V workers.
package vmacl

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

const GroupSID = "S-1-5-83-0"

type Principal uint8

const (
	Other Principal = iota
	WorkerGroup
	Worker
	OtherWorkerPrincipal
)

func Classify(sid *windows.SID) Principal {
	if sid == nil || sid.IdentifierAuthority() != windows.SECURITY_NT_AUTHORITY ||
		sid.SubAuthorityCount() < 2 || sid.SubAuthority(0) != 83 {
		return Other
	}
	if sid.SubAuthorityCount() == 2 && sid.SubAuthority(1) == 0 {
		return WorkerGroup
	}
	if sid.SubAuthorityCount() == 6 && sid.SubAuthority(1) == 1 {
		return Worker
	}
	return OtherWorkerPrincipal
}

const ReadExecute windows.ACCESS_MASK = windows.FILE_GENERIC_READ | windows.FILE_GENERIC_EXECUTE

func ReadOnly(mask windows.ACCESS_MASK) bool {
	const allowed = ReadExecute | windows.GENERIC_READ | windows.GENERIC_EXECUTE
	return mask&^allowed == 0
}

const (
	allowedCompound       = 0x04
	allowedObject         = 0x05
	deniedObject          = 0x06
	allowedCallback       = 0x09
	deniedCallback        = 0x0a
	allowedCallbackObject = 0x0b
	deniedCallbackObject  = 0x0c
)

var deleteAce = windows.NewLazySystemDLL("advapi32.dll").NewProc("DeleteAce")

// RemoveWorkers removes every Hyper-V principal from the DACL.
func RemoveWorkers(path string) error {
	return remove(path, func(principal Principal, _ *windows.ACCESS_ALLOWED_ACE) bool {
		return principal != Other
	})
}

// RemoveGroupWrite removes group entries that carry rights beyond read and execute.
func RemoveGroupWrite(path string) error {
	return remove(path, func(principal Principal, ace *windows.ACCESS_ALLOWED_ACE) bool {
		if principal != WorkerGroup || ReadOnly(ace.Mask) {
			return false
		}
		switch ace.Header.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE, allowedCompound, allowedObject, allowedCallback, allowedCallbackObject:
			return true
		default:
			return false
		}
	})
}

func remove(path string, match func(Principal, *windows.ACCESS_ALLOWED_ACE) bool) error {
	if err := removeEntries(path, match); err != nil {
		return fmt.Errorf("remove Hyper-V access entries from %s: %w", path, err)
	}
	return nil
}

func removeEntries(path string, match func(Principal, *windows.ACCESS_ALLOWED_ACE) bool) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(name, windows.READ_CONTROL|windows.WRITE_DAC|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("unexpected reparse point")
	}
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	defer runtime.KeepAlive(sd)
	acl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if acl == nil {
		return errors.New("unexpected NULL DACL")
	}
	securityInfo := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION)
	var removed []string
	for i := int(acl.AceCount) - 1; i >= 0; i-- {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, uint32(i), &ace); err != nil {
			return err
		}
		sid := aceSID(ace)
		if !match(Classify(sid), ace) {
			continue
		}
		if ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
			// Prevent the parent from reintroducing the removed entry.
			securityInfo |= windows.PROTECTED_DACL_SECURITY_INFORMATION
		}
		removed = append(removed, sid.String())
		if ok, _, err := deleteAce.Call(uintptr(unsafe.Pointer(acl)), uintptr(i)); ok == 0 {
			return err
		}
	}
	if len(removed) == 0 {
		return nil
	}
	if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, securityInfo, nil, nil, acl, nil); err != nil {
		return err
	}
	for _, sid := range removed {
		slog.Info("removed Hyper-V worker access entry", "path", path, "sid", sid)
	}
	return nil
}

// aceSID locates the trustee in a discretionary ACE. Object ACEs can have
// two optional GUIDs between the object flags and the SID.
func aceSID(ace *windows.ACCESS_ALLOWED_ACE) *windows.SID {
	offset := unsafe.Offsetof(ace.SidStart)
	switch ace.Header.AceType {
	case windows.ACCESS_ALLOWED_ACE_TYPE, windows.ACCESS_DENIED_ACE_TYPE, allowedCallback, deniedCallback:
	case allowedCompound:
		offset += 4 // Compound type and reserved word.
	case allowedObject, deniedObject, allowedCallbackObject, deniedCallbackObject:
		flags := ace.SidStart
		offset += 4
		if flags&1 != 0 {
			offset += 16
		}
		if flags&2 != 0 {
			offset += 16
		}
	default:
		return nil
	}
	if offset+8 > uintptr(ace.Header.AceSize) {
		return nil
	}
	return (*windows.SID)(unsafe.Add(unsafe.Pointer(ace), offset))
}
