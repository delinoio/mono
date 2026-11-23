// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
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
	aces, protected, _, release, err := readDACL(path)
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
// another account on a child. An explicit entry of the Hyper-V worker group
// stays, and a service log that another process holds open stays deletable.
func TestOpenCorrectsExistingStateDirACL(t *testing.T) {
	requireTrustedProcess(t)
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(filepath.Join(dir, VMsDir, "sb1"), 0o700); err != nil {
		t.Fatal(err)
	}
	everyone := wellKnown(t, windows.WinWorldSid)
	vmGroup, err := windows.StringToSid(virtualMachinesSID)
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
		if !hasSID(vmAces, vmGroup) {
			t.Errorf("Open %d: the Virtual Machines group lost its entry on the VM directory", i)
		}
		diskAces, _ := dacl(t, disk)
		if !hasSID(diskAces, vmGroup) {
			t.Errorf("Open %d: the VM file does not inherit the Virtual Machines entry", i)
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
	if err := s.WipeLocalData(); err != nil {
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
		{"virtual machines as child owner", p.vmGroup, p.retained, true},
	} {
		if got := ownerAllowed(tc.owner, tc.allowed); got != tc.want {
			t.Errorf("%s: ownerAllowed = %v, want %v", tc.name, got, tc.want)
		}
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
		if sidIn(user, p.trusted) || sidIn(user, p.retained) {
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
		err = checkOwner(dir, p, p.rootOwners)
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
	p, err := loadPrincipals()
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
	// Allow only an unrelated SID, so the real owner of the directory is
	// untrusted whoever it is.
	other := wellKnown(t, windows.WinNullSid)
	err = checkOwner(dir, p, []*windows.SID{other})
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

// Concurrent status readers (LoadStatus reads with os.ReadFile, the default
// Go sharing mode, as `ownsand-agent status` does) never make a write fail
// and never read an incomplete report.
func TestConcurrentStatusReadsDoNotLoseWrites(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Skipf("open the store (needs an elevated administrator): %v", err)
	}
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
		for {
			select {
			case <-stop:
				return
			default:
			}
			var r report
			err := s.LoadStatus(&r)
			// An open that races with the rename may see a sharing error
			// or access denied. It is not an incomplete report, and the
			// next read succeeds, so the test does not count it.
			if err != nil && isSharingError(err) {
				continue
			}
			if err != nil || r.Padding != padding {
				readerErr <- fmt.Errorf("incomplete report: %v (padding %d bytes)", err, len(r.Padding))
				return
			}
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
	if err := s.LoadStatus(&last); err != nil || last.Seq != 300 {
		t.Fatalf("last report %+v, %v", last, err)
	}
}
