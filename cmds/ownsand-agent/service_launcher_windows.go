// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/service"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
)

func windowsLauncherDirectory() (string, error) {
	programFiles, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
	if err != nil {
		return "", err
	}
	return filepath.Join(programFiles, "OwnSand Agent"), nil
}

func launcherPath(dir string) string { return filepath.Join(dir, "ownsand-service.exe") }

func servicePayloads(dir string) service.Payloads {
	return service.Payloads{Dir: dir, Write: store.WriteFileAtomic}
}

func runServiceLauncher(cfg config.Config, args []string, stderr io.Writer) int {
	isService, err := svc.IsWindowsService()
	if err != nil || !isService {
		fmt.Fprintln(stderr, "ownsand-agent: the service control manager starts the launcher")
		return exitUsage
	}
	return runUnderServiceManager(cfg, nil, svc.Run, writeStartupEvent, func(ctx context.Context, cfg config.Config, logFile *os.File) (int, bool) {
		logger := newLogger(cfg)
		dir, err := windowsLauncherDirectory()
		if err != nil {
			logger.Error("find the service launcher directory", "error", err)
			return exitError, false
		}
		payloads := servicePayloads(dir)
		payload, err := payloads.Recover()
		if err != nil {
			logger.Error("recover the service payload", "error", err)
			return exitError, false
		}
		if err := payloads.Prune(payload); err != nil {
			logger.Warn("remove obsolete service payloads", "error", err)
		}
		started, err := payloadFileInfo(payload.Path)
		if err != nil {
			logger.Error("inspect the service payload", "error", err)
			return exitError, false
		}
		code, err := runPayloadProcess(ctx, payload.Path, append([]string{"run"}, args...), logFile)
		if err != nil {
			logger.Error("run the service payload", "error", err)
			return exitError, false
		}
		if code != exitOK || ctx.Err() != nil {
			return code, false
		}
		current, err := payloadFileInfo(payload.Path)
		if err != nil {
			logger.Error("read the published service payload", "error", err)
			return exitError, false
		}
		return code, !os.SameFile(started, current)
	})
}

// Capture the identity through a handle. Stat on the path can defer reading
// the identity until SameFile compares it, after the updater has renamed it.
func payloadFileInfo(path string) (fs.FileInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return file.Stat()
}

// Program Files protects the installation from writes by standard users.
// Limit the launcher and its payloads to SYSTEM and Administrators as well.
func createLauncherDirectory(dir string) error {
	const acl = "O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	descriptor, err := windows.SecurityDescriptorFromString(acl)
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	attributes := windows.SecurityAttributes{
		Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor,
	}
	if err := windows.CreateDirectory(name, &attributes); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	flags, err := windows.GetFileAttributes(name)
	if err != nil {
		return err
	}
	if flags&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || flags&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("the service launcher directory must be a regular directory")
	}
	actual, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	if !matchesLauncherDirectoryACL(actual, descriptor) {
		return errors.New("the service launcher directory must be owned by Administrators and writable only by SYSTEM and Administrators")
	}
	return nil
}

func matchesLauncherDirectoryACL(actual, expected *windows.SECURITY_DESCRIPTOR) bool {
	owner, _, err := actual.Owner()
	wantOwner, _, _ := expected.Owner()
	if err != nil || owner == nil || !owner.Equals(wantOwner) {
		return false
	}
	control, _, err := actual.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return false
	}
	acl, _, err := actual.DACL()
	wantACL, _, _ := expected.DACL()
	if err != nil || acl == nil || acl.AceCount != wantACL.AceCount {
		return false
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var entry, want *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(acl, i, &entry) != nil || windows.GetAce(wantACL, i, &want) != nil ||
			entry.Header != want.Header || entry.Mask != want.Mask ||
			!(*windows.SID)(unsafe.Pointer(&entry.SidStart)).Equals((*windows.SID)(unsafe.Pointer(&want.SidStart))) {
			return false
		}
	}
	return true
}

func lockWindowsLauncher() (string, func(), error) {
	dir, err := windowsLauncherDirectory()
	if err != nil {
		return "", nil, err
	}
	if err := createLauncherDirectory(dir); err != nil {
		return "", nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "setup.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", nil, err
	}
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(lock.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped); err != nil {
		lock.Close()
		return "", nil, fmt.Errorf("lock service setup: %w", err)
	}
	return dir, func() { _ = lock.Close() }, nil
}

func prepareWindowsLauncher(dir, source string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read the trusted service executable: %w", err)
	}
	if err := servicePayloads(dir).Initialize(data); err != nil {
		return err
	}
	return store.WriteFileAtomic(launcherPath(dir), data, 0o700)
}

func prepareNewWindowsService(m *mgr.Mgr, dir, source string) error {
	s, err := m.OpenService(service.Name)
	switch {
	case err == nil:
		s.Close()
		return errors.New("the service already exists; stop it and run ownsand-agent service migrate")
	case !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST):
		return err
	default:
		return prepareWindowsLauncher(dir, source)
	}
}

func serviceLauncherSetup(migrate bool, stdout, stderr io.Writer) int {
	if err := configureWindowsLauncher(migrate); err != nil {
		slog.New(slog.NewJSONHandler(stderr, nil)).Error("service launcher setup failed", "error", err)
		fmt.Fprintln(stderr, "ownsand-agent:", err)
		return exitError
	}
	if migrate {
		fmt.Fprintf(stdout, "The %s service uses the recovery launcher. Start it with:\n  sc.exe start %s\n", service.Name, service.Name)
	} else {
		fmt.Fprintln(stdout, "Prepared the Windows service launcher.")
	}
	return exitOK
}

func configureWindowsLauncher(migrate bool) error {
	source, err := os.Executable()
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to the service manager (run as administrator): %w", err)
	}
	defer m.Disconnect()
	dir, unlock, err := lockWindowsLauncher()
	if err != nil {
		return err
	}
	defer unlock()
	if !migrate {
		return prepareNewWindowsService(m, dir, source)
	}
	s, err := m.OpenService(service.Name)
	if err != nil {
		return err
	}
	defer s.Close()
	cfg, err := s.Config()
	if err != nil {
		return err
	}
	path, alreadyMigrated, err := migratedWindowsServicePath(cfg.BinaryPathName, launcherPath(dir))
	if err != nil || alreadyMigrated {
		return err
	}
	status, err := s.Query()
	if err != nil {
		return err
	}
	if status.State != svc.Stopped {
		return errors.New("stop the ownsand-agent service before migrating it")
	}
	if err := prepareWindowsLauncher(dir, source); err != nil {
		return err
	}
	// Change only ImagePath. Leave the service account, environment and
	// recovery settings in place, including settings installed by an operator.
	return windows.ChangeServiceConfig(s.Handle, windows.SERVICE_NO_CHANGE, windows.SERVICE_NO_CHANGE, windows.SERVICE_NO_CHANGE,
		path, nil, nil, nil, nil, nil, nil)
}

func migratedWindowsServicePath(old, launcher string) (path *uint16, alreadyMigrated bool, err error) {
	args, err := windows.DecomposeCommandLine(old)
	if err != nil {
		return nil, false, err
	}
	if len(args) >= 4 && strings.EqualFold(args[0], launcher) && args[1] == "service" && args[2] == "launch" && args[3] == "run" {
		return nil, true, nil
	}
	if len(args) < 2 || args[1] != "run" {
		return nil, false, errors.New("the existing service executable must use the ownsand-agent run command")
	}
	path, err = windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{launcher, "service", "launch"}, args[1:]...)))
	return path, false, err
}
