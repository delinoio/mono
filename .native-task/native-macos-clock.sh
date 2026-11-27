#!/usr/bin/env bash
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
: "${OWNSAND_VZ_TEST_KERNEL:?set to the Linux arm64 kernel}"
: "${OWNSAND_VZ_TEST_ROOTFS:?set to the Linux arm64 rootfs}"
: "${OWNSAND_VZ_TEST_MACOS_IMAGE:?set to a built and host-compatible macOS image directory}"
test -r "$OWNSAND_VZ_TEST_KERNEL"
test -r "$OWNSAND_VZ_TEST_ROOTFS"
test -d "$OWNSAND_VZ_TEST_MACOS_IMAGE"
export COPYFILE_DISABLE=1 CGO_ENABLED=1 OWNSAND_TEST_VERBOSE=1
export TMPDIR="$PWD/.t3-tmp"
scratch="$TMPDIR/clock-connection-macos"
mkdir -p "$scratch"
package='./cmds/ownsand-agent/internal/vmruntime/vzrt'
tests='^(TestVirtualizationIntegration|TestVirtualizationMacOSIntegration)$'

go vet -mod=readonly "$package"
go test -mod=readonly -race -count=1 -v ./cmds/ownsand-agent/internal/guestclient -run '^TestSetClockOnceCloses' | tee "$scratch/http2.log"
go test -mod=readonly -c -o "$scratch/vzrt.test" "$package"
codesign --force --sign - --entitlements cmds/ownsand-agent/entitlements.plist "$scratch/vzrt.test"
codesign --verify --verbose "$scratch/vzrt.test"
"$scratch/vzrt.test" -test.v -test.timeout=60m -test.run "$tests" 2>&1 | tee "$scratch/vzrt.log"

cat > "$scratch/native_connection_darwin.go" <<'GO'
package vzrt

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestclient"
)

type nativeClockProbeConn struct {
	net.Conn
	rejectWrite bool
	closeOK atomic.Bool
}

func (c *nativeClockProbeConn) Close() error {
	err := c.Conn.Close()
	if err == nil {
		c.closeOK.Store(true)
	}
	return err
}

func (c *nativeClockProbeConn) Write(b []byte) (int, error) {
	if c.rejectWrite {
		return 0, errors.New("native clock probe: injected vsock write failure")
	}
	return c.Conn.Write(b)
}

func probeRestoreClock(ctx context.Context, dial guestclient.Dialer, now time.Time, guest string) error {
	reject := os.Getenv("OWNSAND_NATIVE_CLOCK_PROBE") == "reject"
	var mu sync.Mutex
	var conns []*nativeClockProbeConn
	err := guestclient.SetClockOnce(ctx, func(ctx context.Context) (net.Conn, error) {
		conn, err := dial(ctx)
		if err != nil {
			return nil, err
		}
		c := &nativeClockProbeConn{Conn: conn, rejectWrite: reject}
		mu.Lock()
		conns = append(conns, c)
		mu.Unlock()
		return c, nil
	}, now)
	mu.Lock()
	defer mu.Unlock()
	var closed int
	for _, conn := range conns {
		if conn.closeOK.Load() {
			closed++
		}
	}
	fmt.Printf("NATIVE_CLOCK_PROBE guest=%s dialed=%d closed=%d error=%t reject=%t\n", guest, len(conns), closed, err != nil, reject)
	return err
}
GO

python3 - "$scratch" <<'PY'
import json
import pathlib
import sys

scratch = pathlib.Path(sys.argv[1])
package = pathlib.Path("cmds/ownsand-agent/internal/vmruntime/vzrt").resolve()
overlay = {str(package / "native_connection_probe_darwin.go"): str(scratch / "native_connection_darwin.go")}
for filename, guest in (("runtime_darwin.go", "linux"), ("macos_runtime_darwin.go", "macos")):
    src = package / filename
    dst = scratch / filename
    before = "guestclient.SetClockOnce(clockCtx, v.Dial, time.Now())"
    after = f'probeRestoreClock(clockCtx, v.Dial, time.Now(), "{guest}")'
    code = src.read_text()
    if code.count(before) != 1:
        raise SystemExit(f"expected exactly one clock call in {src}")
    code = code.replace(before, after)
    if code.count("guestclient.") == 0:
        code = code.replace('\t"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestclient"\n', "")
    dst.write_text(code)
    overlay[str(src)] = str(dst)
(scratch / "overlay.json").write_text(json.dumps({"Replace": overlay}))
PY

go test -mod=readonly -overlay "$scratch/overlay.json" -c -o "$scratch/vzrt-probe.test" "$package"
codesign --force --sign - --entitlements cmds/ownsand-agent/entitlements.plist "$scratch/vzrt-probe.test"
codesign --verify --verbose "$scratch/vzrt-probe.test"
for mode in normal reject; do
    log="$scratch/vzrt-$mode.log"
    OWNSAND_NATIVE_CLOCK_PROBE="$mode" "$scratch/vzrt-probe.test" -test.v -test.timeout=60m -test.run "$tests" 2>&1 | tee "$log"
    python3 - "$mode" "$log" <<'PY'
import pathlib
import re
import sys

mode, log = sys.argv[1:]
content = pathlib.Path(log).read_text()
reject = mode == "reject"
observations = re.findall(
    r"NATIVE_CLOCK_PROBE guest=(linux|macos) dialed=(\d+) closed=(\d+) error=(true|false) reject=(true|false)",
    content,
)
if {guest for guest, *_ in observations} != {"linux", "macos"}:
    raise SystemExit(f"both guest types must restore memory state and dial the clock: {observations}")
for guest, dialed, closed, error, injected in observations:
    if int(dialed) == 0 or dialed != closed or (error == "true") != reject or (injected == "true") != reject:
        raise SystemExit(f"unexpected {mode} result for {guest}: {dialed=} {closed=} {error=} {injected=}")
if reject and content.count("set guest clock after memory restore failed") < len(observations):
    raise SystemExit("missing warning after injected clock failure")
print(f"{mode}: Linux and macOS guests both restored from memory and closed every observed clock connection")
PY
done
