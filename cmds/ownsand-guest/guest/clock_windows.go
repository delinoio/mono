// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"fmt"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procSetSystemTime = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetSystemTime")

// enableSystemtime enables SE_SYSTEMTIME_NAME in the process token once. The
// guest helper service runs as LocalSystem, which holds the privilege, but
// SetSystemTime needs it enabled.
var enableSystemtime = sync.OnceValue(func() error {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &token); err != nil {
		return err
	}
	defer token.Close()
	var luid windows.LUID
	name, err := windows.UTF16PtrFromString("SeSystemtimePrivilege")
	if err != nil {
		return err
	}
	if err := windows.LookupPrivilegeValue(nil, name, &luid); err != nil {
		return err
	}
	tp := windows.Tokenprivileges{PrivilegeCount: 1}
	tp.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
	// A token without the privilege still succeeds here (with
	// ERROR_NOT_ALL_ASSIGNED); SetSystemTime then fails with
	// ERROR_PRIVILEGE_NOT_HELD.
	return windows.AdjustTokenPrivileges(token, false, &tp, 0, nil, nil)
})

// setSystemClock sets the system clock (UTC) with SetSystemTime.
func setSystemClock(t time.Time) error {
	if err := enableSystemtime(); err != nil {
		return err
	}
	u := t.UTC()
	st := windows.Systemtime{
		Year: uint16(u.Year()), Month: uint16(u.Month()), DayOfWeek: uint16(u.Weekday()), Day: uint16(u.Day()),
		Hour: uint16(u.Hour()), Minute: uint16(u.Minute()), Second: uint16(u.Second()),
		Milliseconds: uint16(u.Nanosecond() / int(time.Millisecond)),
	}
	if ok, _, e := procSetSystemTime.Call(uintptr(unsafe.Pointer(&st))); ok == 0 {
		return fmt.Errorf("SetSystemTime: %w", e)
	}
	return nil
}
