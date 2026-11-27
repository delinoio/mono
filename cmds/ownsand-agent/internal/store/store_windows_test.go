// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmacl"
)

// requireTrustedProcess skips a test that needs the privileges of the
// agent: the state directory is owned by SYSTEM or Administrators, so a
// process that is not an elevated administrator cannot restrict it.
func requireTrustedProcess(t *testing.T) {
	t.Helper()
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	member, err := windows.Token(0).IsMember(admins)
	if err != nil {
		t.Fatalf("check the Administrators membership: %v", err)
	}
	if !member {
		t.Skip("the test process is not an elevated administrator")
	}
}

func wellKnown(t *testing.T, kind windows.WELL_KNOWN_SID_TYPE) *windows.SID {
	t.Helper()
	sid, err := windows.CreateWellKnownSid(kind)
	if err != nil {
		t.Fatal(err)
	}
	return sid
}

// dacl returns the entries of the DACL of path and whether it is protected.
func dacl(t *testing.T, path string) ([]aceInfo, bool) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	aces, protected, _, release, err := readDACL(path, sd)
	if err != nil {
		t.Fatal(err)
	}
	// The SIDs of the entries point into the security descriptor; copy them.
	out := make([]aceInfo, len(aces))
	for i, a := range aces {
		out[i] = a
		if a.sid != nil {
			c, err := a.sid.Copy()
			if err != nil {
				t.Fatal(err)
			}
			out[i].sid = c
		}
	}
	release()
	return out, protected
}

// assertOnlyTrusted fails unless every entry of aces grants access to
// SYSTEM or Administrators only, and both are present.
func assertOnlyTrusted(t *testing.T, path string, aces []aceInfo) {
	t.Helper()
	system := wellKnown(t, windows.WinLocalSystemSid)
	admins := wellKnown(t, windows.WinBuiltinAdministratorsSid)
	seenSystem, seenAdmins := false, false
	for _, a := range aces {
		if a.aceType != windows.ACCESS_ALLOWED_ACE_TYPE || a.sid == nil {
			t.Errorf("%s: unexpected ACE type %d", path, a.aceType)
			continue
		}
		switch {
		case a.sid.Equals(system):
			seenSystem = true
		case a.sid.Equals(admins):
			seenAdmins = true
		default:
			t.Errorf("%s: ACE for %s", path, a.sid)
		}
	}
	if !seenSystem || !seenAdmins {
		t.Errorf("%s: SYSTEM present %v, Administrators present %v", path, seenSystem, seenAdmins)
	}
}

// addExplicitACE adds an explicit allowed entry for sid to the DACL of path.
func addExplicitACE(t *testing.T, path string, sid *windows.SID, mask windows.ACCESS_MASK, inherit uint32) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	old, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: mask,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inherit,
		Trustee:           trustee(sid),
	}}, old)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
}

func hasSID(aces []aceInfo, sid *windows.SID) bool {
	for _, a := range aces {
		if a.sid != nil && a.sid.Equals(sid) {
			return true
		}
	}
	return false
}

// A fresh state directory is protected, and the files that the store
// creates later inherit the two entries.
func TestOpenRestrictsStateDirACL(t *testing.T) {
	requireTrustedProcess(t)
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveIdentity(&Identity{DeviceID: "d", PrivateKeySeed: make([]byte, 32)}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSandbox("sb1", map[string]string{"id": "sb1"}); err != nil {
		t.Fatal(err)
	}

	aces, protected := dacl(t, dir)
	if !protected {
		t.Error("the DACL of the state directory inherits from its parent")
	}
	assertOnlyTrusted(t, dir, aces)
	if len(aces) != 2 {
		t.Errorf("state directory has %d ACEs, want 2", len(aces))
	}
	for _, rel := range []string{IdentityFile, SandboxesDir, filepath.Join(SandboxesDir, "sb1.json"), VMsDir} {
		path := filepath.Join(dir, rel)
		aces, _ := dacl(t, path)
		assertOnlyTrusted(t, path, aces)
		for _, a := range aces {
			if !a.inherited {
				t.Errorf("%s: explicit ACE for %s, want inherited", path, a.sid)
			}
		}
	}
	for _, sid := range []*windows.SID{
		wellKnown(t, windows.WinBuiltinUsersSid),
		wellKnown(t, windows.WinWorldSid),
		wellKnown(t, windows.WinAuthenticatedUserSid),
	} {
		if hasSID(aces, sid) {
			t.Errorf("state directory grants access to %s", sid)
		}
	}
}

// Open corrects a state directory that exists with a wider ACL: inherited
// entries of the parent, a protected child and an explicit entry of
// another account on a child. The Hyper-V group's inheritable grant is
// removed, and a service log that another process holds open stays deletable.
func TestOpenCorrectsExistingStateDirACL(t *testing.T) {
	requireTrustedProcess(t)
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(filepath.Join(dir, VMsDir, "sb1"), 0o700); err != nil {
		t.Fatal(err)
	}
	everyone := wellKnown(t, windows.WinWorldSid)
	vmGroup, err := windows.StringToSid(vmacl.GroupSID)
	if err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(dir, IdentityFile)
	if err := os.WriteFile(identity, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	addExplicitACE(t, identity, everyone, 0x001200A9, windows.NO_INHERITANCE)
	vmDir := filepath.Join(dir, VMsDir, "sb1")
	addExplicitACE(t, vmDir, vmGroup, fileAllAccess, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT)
	disk := filepath.Join(vmDir, "disk.vhdx")
	if err := os.WriteFile(disk, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A protected child: it ignores the parent until Open resets it.
	protectedFile := filepath.Join(dir, "protected.json")
	if err := os.WriteFile(protectedFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: fileAllAccess,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee:           trustee(everyone),
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(protectedFile, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	// The service log, held open while Open restricts the directory.
	logPath := filepath.Join(dir, "agent.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()

	// The parent chain grants Users and more: the state directory is a
	// child of the test temporary directory, which inherits user entries.
	before, _ := dacl(t, dir)
	if len(before) == 0 {
		t.Fatal("the test directory has no ACL entries")
	}

	for i := 0; i < 2; i++ { // the second Open leaves the corrected ACL alone
		if _, err := Open(dir); err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
		aces, protected := dacl(t, dir)
		if !protected || len(aces) != 2 {
			t.Fatalf("Open %d: root protected %v with %d ACEs", i, protected, len(aces))
		}
		assertOnlyTrusted(t, dir, aces)

		idAces, _ := dacl(t, identity)
		assertOnlyTrusted(t, identity, idAces)
		if hasSID(idAces, everyone) {
			t.Errorf("Open %d: identity.json still grants Everyone", i)
		}
		pAces, pProtected := dacl(t, protectedFile)
		if pProtected || hasSID(pAces, everyone) {
			t.Errorf("Open %d: protected child protected=%v everyone=%v", i, pProtected, hasSID(pAces, everyone))
		}
		assertOnlyTrusted(t, protectedFile, pAces)

		vmAces, _ := dacl(t, vmDir)
		diskAces, _ := dacl(t, disk)
		if hasSID(vmAces, vmGroup) || hasSID(diskAces, vmGroup) {
			t.Errorf("Open %d: the legacy worker group grant remains on the VM directory or disk", i)
		}
		if hasSID(diskAces, everyone) || hasSID(vmAces, everyone) {
			t.Errorf("Open %d: a VM entry grants Everyone", i)
		}
	}

	// A device reset and a service removal delete everything, the open log
	// included, and the identity key too.
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := logFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Reset(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	for _, p := range []string{identity, logPath, disk} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists after Reset: %v", p, err)
		}
	}
}

// Wiping the local data recreates the subdirectories with the inherited
// entries.
func TestWipeLocalDataKeepsACL(t *testing.T) {
	requireTrustedProcess(t)
	s, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := wipeLocalData(t, s); err != nil {
		t.Fatal(err)
	}
	aces, _ := dacl(t, s.Path(VMsDir))
	assertOnlyTrusted(t, s.Path(VMsDir), aces)
}

func TestOwnerAllowed(t *testing.T) {
	p, err := loadPrincipals()
	if err != nil {
		t.Fatal(err)
	}
	users := wellKnown(t, windows.WinBuiltinUsersSid)
	everyone := wellKnown(t, windows.WinWorldSid)
	for _, tc := range []struct {
		name    string
		owner   *windows.SID
		allowed []*windows.SID
		want    bool
	}{
		{"system", p.system, p.trusted, true},
		{"administrators", p.admins, p.trusted, true},
		{"users", users, p.trusted, false},
		{"everyone", everyone, p.trusted, false},
		{"nil owner", nil, p.trusted, false},
		{"virtual machines as root owner", p.vmGroup, p.trusted, false},
		{"virtual machines as child owner", p.vmGroup, p.childOwners, true},
	} {
		if got := ownerAllowed(tc.owner, tc.allowed); got != tc.want {
			t.Errorf("%s: ownerAllowed = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestOpenKeepsPerVMEntriesAndDropsGroupWrite(t *testing.T) {
	requireTrustedProcess(t)
	dir := filepath.Join(t.TempDir(), "state")
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	vmGroup, err := windows.StringToSid(vmacl.GroupSID)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := windows.StringToSid("S-1-5-83-1-1-2-3-4")
	if err != nil {
		t.Fatal(err)
	}
	image := st.Path(ImagesDir, images.WindowsDir)
	vmDisk := st.Path(VMsDir, "sandbox", "disk.vhdx")
	imageDisk := filepath.Join(image, images.WindowsDisk)
	buildDisk := filepath.Join(images.BuildDir(image), images.WindowsDisk)
	kernel := st.Path(ImagesDir, "linux", "vmlinux")
	for _, path := range []string{vmDisk, imageDisk, buildDisk, kernel} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	addExplicitACE(t, vmDisk, worker, fileAllAccess, windows.NO_INHERITANCE)
	addExplicitACE(t, buildDisk, worker, fileAllAccess, windows.NO_INHERITANCE)
	addExplicitACE(t, imageDisk, worker, fileAllAccess, windows.NO_INHERITANCE)
	addExplicitACE(t, imageDisk, vmGroup, fileAllAccess, windows.NO_INHERITANCE)
	addExplicitACE(t, kernel, vmGroup, vmacl.ReadExecute, windows.NO_INHERITANCE)
	lock, err := images.TryLock(image)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if lock != nil {
			_ = lock.Release()
		}
	}()
	checkACE := func(path string, sid *windows.SID, mask windows.ACCESS_MASK, want bool) {
		t.Helper()
		aces, _ := dacl(t, path)
		for _, a := range aces {
			if a.sid != nil && a.sid.Equals(sid) {
				if !want || a.aceType != windows.ACCESS_ALLOWED_ACE_TYPE || a.inherited || a.mask != mask {
					t.Errorf("%s: unexpected ACE for %v with mask %#x", path, sid, a.mask)
				}
				return
			}
		}
		if want {
			t.Errorf("%s: missing ACE for %v", path, sid)
		}
	}
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	checkACE(vmDisk, worker, fileAllAccess, true)
	checkACE(buildDisk, worker, fileAllAccess, true)
	checkACE(imageDisk, worker, 0, false)
	checkACE(imageDisk, vmGroup, 0, false)
	checkACE(kernel, vmGroup, vmacl.ReadExecute, true)

	err = lock.Release()
	lock = nil
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	checkACE(buildDisk, worker, 0, false)
	checkACE(vmDisk, worker, fileAllAccess, true)
}

func TestVMWorkerACERetention(t *testing.T) {
	p, err := newPrincipals(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := windows.StringToSid("S-1-5-83-1-1-2-3-4")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		sid        *windows.SID
		mask       windows.ACCESS_MASK
		flags      uint8
		keepWorker bool
		want       bool
	}{
		{"disk worker", worker, fileAllAccess, 0, true, true},
		{"worker outside vms", worker, fileAllAccess, 0, false, false},
		{"kernel group", p.vmGroup, vmacl.ReadExecute, 0, false, true},
		{"generic read group", p.vmGroup, windows.GENERIC_READ, 0, false, true},
		{"generic write group", p.vmGroup, windows.GENERIC_WRITE, 0, true, false},
		{"delete group", p.vmGroup, windows.DELETE, 0, true, false},
		{"change ACL group", p.vmGroup, windows.WRITE_DAC, 0, true, false},
		{"inheritable group", p.vmGroup, vmacl.ReadExecute, windows.OBJECT_INHERIT_ACE, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := aceInfo{aceType: windows.ACCESS_ALLOWED_ACE_TYPE, flags: tc.flags, mask: tc.mask, sid: tc.sid}
			if got := p.keepACE(a, tc.keepWorker); got != tc.want {
				t.Errorf("keepACE = %v, want %v", got, tc.want)
			}
		})
	}
}

// The user of an elevated administrator process may own the state directory
// and its entries (the account that the hosted Windows CI runner uses owns
// its temporary directories), and no process gets that allowance for another
// account or without elevation.
func TestPrincipalsProcessUserOwner(t *testing.T) {
	user := wellKnown(t, windows.WinBuiltinGuestsSid) // stands for the process user
	other := wellKnown(t, windows.WinBuiltinUsersSid) // stands for another account
	for _, tc := range []struct {
		name     string
		elevated bool
		owner    *windows.SID
		child    bool
		want     bool
	}{
		{"elevated, root owned by the process user", true, user, false, true},
		{"elevated, child owned by the process user", true, user, true, true},
		{"not elevated, root owned by the process user", false, user, false, false},
		{"not elevated, child owned by the process user", false, user, true, false},
		{"elevated, root owned by another account", true, other, false, false},
		{"elevated, child owned by another account", true, other, true, false},
		{"not elevated, root owned by another account", false, other, false, false},
	} {
		p, err := newPrincipals(user, tc.elevated)
		if err != nil {
			t.Fatal(err)
		}
		allowed := p.rootOwners
		if tc.child {
			allowed = p.childOwners
		}
		if got := ownerAllowed(tc.owner, allowed); got != tc.want {
			t.Errorf("%s: ownerAllowed = %v, want %v", tc.name, got, tc.want)
		}
		// The owner allowance never adds an entry to a DACL or keeps an
		// explicit entry of the process user on a child.
		if p.keepACE(aceInfo{aceType: windows.ACCESS_ALLOWED_ACE_TYPE, sid: user, mask: fileAllAccess}, true) {
			t.Errorf("%s: the process user is a trusted or retained principal", tc.name)
		}
	}
	// SYSTEM and Administrators stay accepted without elevation.
	p, err := newPrincipals(user, false)
	if err != nil {
		t.Fatal(err)
	}
	if !ownerAllowed(p.system, p.rootOwners) || !ownerAllowed(p.admins, p.rootOwners) {
		t.Error("SYSTEM or Administrators is not an allowed root owner")
	}
}

// A directory that the test process owns passes the owner check only when
// the process is treated as elevated: the check reads the real owner of a
// real directory with the real process user.
func TestCheckOwnerProcessUser(t *testing.T) {
	user, _, err := processIdentity()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		t.Fatal(err)
	}
	if base, err := newPrincipals(user, false); err != nil {
		t.Fatal(err)
	} else if sidIn(user, base.trusted) {
		t.Skipf("the process user %s is SYSTEM or Administrators, which are always trusted", user)
	}
	if !owner.Equals(user) {
		// An elevated administrator usually creates objects that the
		// Administrators group owns; only a user-owned directory proves
		// the process-user rule.
		t.Skipf("the temporary directory is owned by %s, not by the process user %s", owner, user)
	}
	for _, elevated := range []bool{true, false} {
		p, err := newPrincipals(user, elevated)
		if err != nil {
			t.Fatal(err)
		}
		h, _, err := openStateEntry(dir, windows.READ_CONTROL)
		if err != nil {
			t.Fatal(err)
		}
		err = checkHandleOwner(dir, h, p.rootOwners)
		windows.CloseHandle(h)
		if elevated && err != nil {
			t.Errorf("elevated: owner %s rejected: %v", owner, err)
		}
		if !elevated && err == nil {
			t.Errorf("not elevated: owner %s accepted", owner)
		}
	}
}

// Open fails closed, with a message that names the owner, when the owner of
// the state directory is not trusted.
func TestCheckOwnerRejectsUntrustedOwner(t *testing.T) {
	dir := t.TempDir()
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		t.Fatal(err)
	}
	// Allow only an unrelated SID, so the real owner of the directory is
	// untrusted whoever it is.
	other := wellKnown(t, windows.WinNullSid)
	h, _, err := openStateEntry(dir, windows.READ_CONTROL)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	err = checkHandleOwner(dir, h, []*windows.SID{other})
	if err == nil {
		t.Fatalf("owner %s accepted", owner)
	}
	if !strings.Contains(err.Error(), "must be owned by") || !strings.Contains(err.Error(), owner.String()) {
		t.Errorf("error does not name the owner and the rule: %v", err)
	}
}

// openHeld opens path with the given share mode and keeps it open, as a
// concurrent reader does.
func openHeld(t *testing.T, path string, share uint32) windows.Handle {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ, share, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	return h
}

const shareReadWrite = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE

// A reader that holds the file open (os.Open and os.ReadFile, an older CLI,
// antivirus) blocks the rename. The replacement succeeds when the
// reader closes within the retry window.
func TestWriteFileAtomicRetriesWhileReaderHoldsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	if err := WriteFileAtomic(path, []byte("old report"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := openHeld(t, path, shareReadWrite)
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(200 * time.Millisecond)
		windows.CloseHandle(h)
	}()
	err := WriteFileAtomic(path, []byte("new report"), 0o600)
	<-released
	if err != nil {
		t.Fatalf("replace after the reader closed: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "new report" {
		t.Fatalf("read after replace: %q, %v", got, err)
	}
}

// A reader that never closes makes the replacement fail after the bounded
// retry, keeps the old file complete and leaves no temporary file.
func TestWriteFileAtomicGivesUpOnStuckReader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "status.json")
	if err := WriteFileAtomic(path, []byte("old report"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := openHeld(t, path, shareReadWrite)
	err := WriteFileAtomic(path, []byte("new report"), 0o600)
	windows.CloseHandle(h)
	if err == nil {
		t.Fatal("replace succeeded while a handle was open")
	}
	if !isSharingError(err) {
		t.Fatalf("unexpected error: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "old report" {
		t.Fatalf("old file after failed replace: %q, %v", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v, %v", entries, err)
	}
}

// The status command uses a separate Store from the writer. The test only
// needs a writable directory, so it also runs without elevation.
func TestConcurrentStatusReadsDoNotLoseWrites(t *testing.T) {
	dir := t.TempDir()
	s := &Store{dir: dir}
	reader := &Store{dir: dir}
	type report struct {
		Seq     int    `json:"seq"`
		Padding string `json:"padding"`
	}
	padding := strings.Repeat("x", 4096)
	if err := s.SaveStatus(report{Seq: 0, Padding: padding}); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	readerErr := make(chan error, 1)
	go func() {
		defer close(readerErr)
		last := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			var r report
			err := reader.LoadStatus(&r)
			if err != nil || r.Padding != padding || r.Seq < last || r.Seq > 300 {
				readerErr <- fmt.Errorf("invalid report: %v (sequence %d, previous %d, padding %d bytes)", err, r.Seq, last, len(r.Padding))
				return
			}
			last = r.Seq
		}
	}()
	for i := 1; i <= 300; i++ {
		if err := s.SaveStatus(report{Seq: i, Padding: padding}); err != nil {
			close(stop)
			t.Fatalf("write %d failed: %v", i, err)
		}
	}
	close(stop)
	if err := <-readerErr; err != nil {
		t.Fatal(err)
	}
	var last report
	if err := reader.LoadStatus(&last); err != nil || last.Seq != 300 || last.Padding != padding {
		t.Fatalf("last report: %v (sequence %d, padding %d bytes)", err, last.Seq, len(last.Padding))
	}
}

func junction(t *testing.T, path, target string) {
	t.Helper()
	output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", path, target).CombinedOutput()
	if err != nil {
		t.Fatalf("create fixture junction: %v: %s", err, output)
	}
	t.Cleanup(func() {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove fixture junction: %v", err)
		}
	})
}

func requireStatePathReason(t *testing.T, err error, path string, reason StatePathReason) {
	t.Helper()
	var rejected *StatePathError
	if !errors.Is(err, ErrUntrustedStateDir) || !errors.As(err, &rejected) || rejected.Path != path || rejected.Reason != reason {
		t.Fatalf("state path error = %v, want %s at %q", err, reason, path)
	}
}

func securityDescriptorString(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd.String()
}

func TestWindowsStateRejectsRootJunction(t *testing.T) {
	target := t.TempDir()
	sentinel := filepath.Join(target, IdentityFile)
	if err := os.WriteFile(sentinel, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := securityDescriptorString(t, target)
	root := filepath.Join(t.TempDir(), "state")
	junction(t, root, target)

	requireStatePathReason(t, PrepareDir(root), root, StatePathReparsePoint)
	s, err := OpenForRead(root)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := s.LockImageBuild(t.Context())
	if unlock != nil {
		unlock()
	}
	requireStatePathReason(t, err, root, StatePathReparsePoint)
	_, err = Open(root)
	requireStatePathReason(t, err, root, StatePathReparsePoint)
	requireStatePathReason(t, ResetDir(root), root, StatePathReparsePoint)

	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 1 || entries[0].Name() != IdentityFile {
		t.Fatalf("junction target entries = %v, %v", entries, err)
	}
	if b, err := os.ReadFile(sentinel); err != nil || string(b) != "fixture" {
		t.Fatalf("junction target sentinel = %q, %v", b, err)
	}
	if after := securityDescriptorString(t, target); before != after {
		t.Fatal("junction target security descriptor changed")
	}
}

func TestWindowsStateRejectsUntrustedRootOwner(t *testing.T) {
	user, elevated, err := processIdentity()
	if err != nil {
		t.Fatal(err)
	}
	p, err := newPrincipals(user, elevated)
	if err != nil {
		t.Fatal(err)
	}
	if ownerAllowed(user, p.rootOwners) {
		t.Skip("requires an account outside the list of trusted owners")
	}

	root := t.TempDir()
	before := securityDescriptorString(t, root)
	requireStatePathReason(t, PrepareDir(root), root, StatePathUntrustedOwner)
	_, err = Open(root)
	requireStatePathReason(t, err, root, StatePathUntrustedOwner)
	requireStatePathReason(t, ResetDir(root), root, StatePathUntrustedOwner)
	if after := securityDescriptorString(t, root); before != after {
		t.Fatal("untrusted root security descriptor changed")
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("untrusted root entries = %v, %v", entries, err)
	}
}

func TestWindowsStateRejectsFixedDirectoryJunctionBeforeOpen(t *testing.T) {
	testFixedDirectoryJunctions(t, false)
}

func TestOpenRejectsFixedSubdirectoryJunction(t *testing.T) {
	requireTrustedProcess(t)
	testFixedDirectoryJunctions(t, true)
}

func testFixedDirectoryJunctions(t *testing.T, fullOpen bool) {
	t.Helper()
	for _, sub := range stateSubdirs {
		t.Run(sub, func(t *testing.T) {
			root, target := t.TempDir(), t.TempDir()
			before := securityDescriptorString(t, target)
			path := filepath.Join(root, sub)
			junction(t, path, target)

			var err error
			if fullOpen {
				_, err = Open(root)
			} else {
				err = prepareStateSubdir(path)
			}
			requireStatePathReason(t, err, path, StatePathReparsePoint)
			if fullOpen {
				if err := ResetDir(root); err != nil {
					t.Fatalf("ResetDir with damaged layout: %v", err)
				}
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("junction after ResetDir = %v, want absent", err)
				}
			}
			if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
				t.Fatalf("junction target entries = %v, %v", entries, err)
			}
			if after := securityDescriptorString(t, target); before != after {
				t.Fatal("junction target security descriptor changed")
			}
		})
	}
}

func TestWindowsStateChecksDeepJunctionOwner(t *testing.T) {
	root, target := t.TempDir(), t.TempDir()
	content := filepath.Join(root, SnapshotsDir, "record", "content")
	if err := os.MkdirAll(content, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(content, "link")
	junction(t, path, target)
	targetBefore := securityDescriptorString(t, target)
	readJunction := func() (uint32, *windows.SECURITY_DESCRIPTOR) {
		t.Helper()
		h, info, err := openStateEntry(path, windows.READ_CONTROL)
		if err != nil {
			t.Fatal(err)
		}
		defer windows.CloseHandle(h)
		sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatalf("read junction security descriptor: %v", err)
		}
		return info.FileAttributes, sd
	}
	attributesBefore, sd := readJunction()
	if attributesBefore&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		t.Fatalf("fixture is missing the reparse point attribute: %#x", attributesBefore)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		t.Fatal(err)
	}
	p, err := loadPrincipals()
	if err != nil {
		t.Fatal(err)
	}
	p.childOwners = []*windows.SID{wellKnown(t, windows.WinNullSid)}
	_, err = resetChild(path, p, false)
	requireStatePathReason(t, err, path, StatePathUntrustedOwner)
	var rejection *StatePathError
	if !errors.As(err, &rejection) || rejection.OwnerSID != owner.String() {
		t.Fatalf("junction owner = %v, want %s", err, owner)
	}
	p.childOwners = []*windows.SID{owner}
	reparse, err := resetChild(path, p, false)
	if err != nil || !reparse {
		t.Fatalf("reset trusted junction: reparse point %v, error %v", reparse, err)
	}
	attributes, after := readJunction()
	if attributes != attributesBefore {
		t.Fatalf("resetChild changed junction attributes from %#x to %#x", attributesBefore, attributes)
	}
	if after.String() != sd.String() {
		t.Fatalf("resetChild changed the junction security descriptor from %q to %q", sd.String(), after.String())
	}

	// Windows can update inherited ACEs on the junction when the walk
	// repairs an ancestor's DACL.
	if err := resetChildren(root, p); err != nil {
		t.Fatalf("walk with a trusted junction: %v", err)
	}
	h, info, err := openStateEntry(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	windows.CloseHandle(h)
	if info.FileAttributes != attributesBefore {
		t.Fatalf("walk changed junction attributes from %#x to %#x", attributesBefore, info.FileAttributes)
	}
	if after := securityDescriptorString(t, target); targetBefore != after {
		t.Fatalf("walk changed the junction target security descriptor from %q to %q", targetBefore, after)
	}
}

func TestOpenKeepsTrustedDeepJunction(t *testing.T) {
	requireTrustedProcess(t)
	root, target := t.TempDir(), t.TempDir()
	if _, err := Open(root); err != nil {
		t.Fatal(err)
	}
	content := filepath.Join(root, SnapshotsDir, "record", "content")
	if err := os.MkdirAll(content, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(content, "link")
	junction(t, path, target)
	before := securityDescriptorString(t, target)
	if _, err := Open(root); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode().IsRegular() {
		t.Fatalf("trusted junction was lost: %v, %v", info, err)
	}
	if after := securityDescriptorString(t, target); before != after {
		t.Fatal("Open changed the deep junction target security descriptor")
	}
	if err := ResetDir(root); err != nil {
		t.Fatalf("ResetDir with a deep junction: %v", err)
	}
	if after := securityDescriptorString(t, target); before != after {
		t.Fatal("ResetDir changed the deep junction target security descriptor")
	}
}

func TestOpenEnrollmentLockAcquiresRegularFile(t *testing.T) {
	root := t.TempDir()
	release, err := acquireEnrollmentLock(t.Context(), root, enrollmentExclusive)
	if err != nil {
		t.Fatal(err)
	}
	release()

	release, err = acquireEnrollmentLock(t.Context(), root, enrollmentExclusive)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestOpenEnrollmentLockRejectsReparsePoint(t *testing.T) {
	root, target := t.TempDir(), t.TempDir()
	path := filepath.Join(root, EnrollmentLockFile)
	junction(t, path, target)
	before := securityDescriptorString(t, target)
	if f, err := openEnrollmentLock(root); err == nil || f != nil {
		if f != nil {
			f.Close()
		}
		t.Fatalf("openEnrollmentLock on a junction = %v, %v", f, err)
	}
	if after := securityDescriptorString(t, target); before != after {
		t.Fatal("enrollment lock changed the junction target security descriptor")
	}
}

func TestOpenEnrollmentLockRejectsFileSymlink(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, EnrollmentLockFile)
	target := filepath.Join(t.TempDir(), "fixture")
	if err := os.WriteFile(target, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			t.Skipf("file symlink creation is unavailable: %v", err)
		}
		t.Fatal(err)
	}
	f, err := openEnrollmentLock(root)
	if f != nil {
		f.Close()
		t.Fatal("openEnrollmentLock returned a handle for a symlink")
	}
	requireStatePathReason(t, err, path, StatePathReparsePoint)
}

func TestOpenEnrollmentLockRejectsHardLink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "fixture")
	if err := os.WriteFile(target, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, EnrollmentLockFile)
	if err := os.Link(target, path); err != nil {
		t.Fatal(err)
	}
	_, err := acquireEnrollmentLock(t.Context(), root, enrollmentExclusive)
	requireStatePathReason(t, err, path, StatePathMultipleLinks)
	if b, err := os.ReadFile(target); err != nil || string(b) != "fixture" {
		t.Fatalf("hard-link target = %q, %v", b, err)
	}
}
