// SPDX-License-Identifier: FSL-1.1-ALv2

package vzrt_test

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func execSucceeded(exited *ownsandv1.ExecExited) bool {
	code, ok := exited.GetStatus().(*ownsandv1.ExecExited_ExitCode)
	return ok && code != nil && code.ExitCode == 0
}

func checkMacOSNoneInterfaces(out string, exited *ownsandv1.ExecExited, guestMAC string) error {
	if !execSucceeded(exited) {
		return fmt.Errorf("NONE sandbox ifconfig -a failed: terminal event %q, output %q", exited, out)
	}
	if strings.Contains(out, guestMAC) {
		return fmt.Errorf("NONE sandbox has a network device: %q", out)
	}
	return nil
}

func TestMacOSNoneInterfaceProbe(t *testing.T) {
	const (
		guestMAC = "02:00:00:00:00:01"
		loopback = "lo0: flags=8049<UP,LOOPBACK,RUNNING,MULTICAST> mtu 16384\n"
	)
	success := &ownsandv1.ExecExited{Status: &ownsandv1.ExecExited_ExitCode{ExitCode: 0}}
	tests := []struct {
		name    string
		out     string
		exited  *ownsandv1.ExecExited
		wantErr string
	}{
		{
			name:   "loopback only",
			out:    loopback,
			exited: success,
		},
		{
			name:    "guest MAC present",
			out:     loopback + "en0: flags=8863<UP,BROADCAST,RUNNING> mtu 1500\n\tether " + guestMAC + "\n",
			exited:  success,
			wantErr: "NONE sandbox has a network device",
		},
		{
			name:    "nonzero exit",
			out:     "ifconfig: interface listing failed\n",
			exited:  &ownsandv1.ExecExited{Status: &ownsandv1.ExecExited_ExitCode{ExitCode: 1}},
			wantErr: `terminal event "exit_code:1"`,
		},
		{
			name:    "signal",
			out:     loopback,
			exited:  &ownsandv1.ExecExited{Status: &ownsandv1.ExecExited_Signal{Signal: 15}},
			wantErr: `terminal event "signal:15"`,
		},
		{
			name:    "unset status",
			out:     loopback,
			exited:  &ownsandv1.ExecExited{},
			wantErr: `terminal event ""`,
		},
		{
			name:    "missing terminal event",
			out:     loopback,
			wantErr: `terminal event "<nil>"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkMacOSNoneInterfaces(tt.out, tt.exited, guestMAC)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("interface probe failed: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), strconv.Quote(tt.out)) {
				t.Fatalf("interface probe error = %v, want %q and output %q", err, tt.wantErr, tt.out)
			}
		})
	}
}
