#!/bin/sh
set -eu

platform=$1
expected_sha=$2
actual_sha=$(git rev-parse HEAD)
if [ "$actual_sha" != "$expected_sha" ]; then
    printf 'Expected revision %s, got %s\n' "$expected_sha" "$actual_sha" >&2
    exit 1
fi
if [ "$(go env GOHOSTOS)" != "$platform" ]; then
    printf 'Run on %s\n' "$platform" >&2
    exit 1
fi
printf 'Revision %s\n' "$actual_sha"
uname -srm
go version

package=./cmds/ownsand-guest/guestlisten
logs=.t3-tmp/vsock-followup-validation
mkdir -p "$logs"
TMPDIR="$PWD/.t3-tmp"
export TMPDIR

run_positive() {
    selected=$1
    shift
    if ! go test -mod=readonly -v -count=1 -timeout=30s "$package" -run "$selected" > "$logs/positive.log" 2>&1; then
        cat "$logs/positive.log"
        exit 1
    fi
    cat "$logs/positive.log"
    if grep -Fq -- '--- SKIP:' "$logs/positive.log"; then
        printf 'The positive run requires native vsock support.\n' >&2
        exit 1
    fi
    for test_name in "$@"; do
        grep -Fq -- "--- PASS: $test_name (" "$logs/positive.log"
    done
}

run_overlay() {
    label=$1 source_file=$2 selected=$3 expected_status=$4 expected_result=$5 diagnostic=$6 needle=$7 replacement=$8
    copy="$logs/$label.go"
    overlay="$logs/$label.json"
    log="$logs/$label.log"
    awk -v needle="$needle" -v replacement="$replacement" '
        index($0, needle) {
            ++matches
            start = index($0, needle)
            $0 = substr($0, 1, start - 1) replacement substr($0, start + length(needle))
        }
        { print }
        END { if (matches != 1) exit 1 }
    ' "$source_file" > "$copy"
    printf '{"Replace":{"%s":"%s"}}\n' "$source_file" "$copy" > "$overlay"
    result=0
    go test -mod=readonly -v -count=1 -timeout=30s -overlay "$overlay" "$package" -run "$selected" > "$log" 2>&1 || result=$?
    cat "$log"
    if [ "$result" -ne "$expected_status" ]; then
        printf '%s expected exit %s, got %s\n' "$label" "$expected_status" "$result" >&2
        exit 1
    fi
    grep -F -- "$expected_result" "$log"
    grep -F -- "$diagnostic" "$log"
}

unix_test="$package/vsock_unix_test.go"
linux_test="$package/vsock_linux_test.go"
raw_open='fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)'
probe_connect='if err := unix.Connect(client, &unix.SockaddrVM{CID: unix.VMADDR_CID_LOCAL, Port: bound.Port}); err != nil {'

case "$platform" in
    linux)
        run_positive '^(TestVsockLoopback|TestSocketCloexec|TestCreateCloexecMarkFailureClosesDescriptor)$' \
            TestVsockLoopback TestVsockLoopback/single_write TestVsockLoopback/split_writes \
            TestSocketCloexec TestCreateCloexecMarkFailureClosesDescriptor
        run_overlay no-family "$unix_test" '^(TestSocketCloexec|TestVsockLoopback)$' 0 '--- SKIP: TestSocketCloexec' \
            'raw AF_VSOCK stream socket unsupported:' "$raw_open" 'fd, err := -1, error(unix.EAFNOSUPPORT)'
        grep -F -- '--- SKIP: TestVsockLoopback' "$logs/no-family.log"
        run_overlay no-device "$unix_test" '^(TestSocketCloexec|TestVsockLoopback)$' 0 '--- SKIP: TestSocketCloexec' \
            'raw AF_VSOCK stream socket device unavailable:' "$raw_open" 'fd, err := -1, error(unix.ENODEV)'
        grep -F -- '--- SKIP: TestVsockLoopback' "$logs/no-device.log"
        run_overlay restricted-socket "$unix_test" '^TestSocketCloexec$' 0 '--- SKIP: TestSocketCloexec' \
            'raw AF_VSOCK stream socket denied:' "$raw_open" 'fd, err := -1, error(unix.EPERM)'
        run_overlay raw-socket-io-error "$unix_test" '^TestSocketCloexec$' 1 '--- FAIL: TestSocketCloexec' \
            'open raw AF_VSOCK stream socket: input/output error' "$raw_open" 'fd, err := -1, error(unix.EIO)'
        run_overlay no-local-transport "$linux_test" '^TestVsockLoopback$' 0 '--- SKIP: TestVsockLoopback' \
            'VMADDR_CID_LOCAL, local transport unavailable:' "$probe_connect" 'if err := error(unix.ENODEV); err != nil {'
        run_overlay probe-connect-refused "$linux_test" '^TestVsockLoopback$' 1 '--- FAIL: TestVsockLoopback' \
            'raw vsock probe connect to VMADDR_CID_LOCAL: connection refused' "$probe_connect" 'if err := error(unix.ECONNREFUSED); err != nil {'
        run_overlay listener-io-error "$linux_test" '^TestVsockLoopback$' 1 '--- FAIL: TestVsockLoopback/single_write' \
            'listenVsock: input/output error' 'l, err := listenVsock(basePort + uint32(i))' \
            'l, err := listenVsock(basePort + uint32(i)); if err == nil { l.Close(); err = unix.EIO }'
        grep -F -- '--- FAIL: TestVsockLoopback/split_writes' "$logs/listener-io-error.log"
        run_overlay client-socket-io-error "$linux_test" '^TestVsockLoopback$' 1 '--- FAIL: TestVsockLoopback/single_write' \
            'open vsock loopback client socket: input/output error' \
            'fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)' 'fd, err := -1, error(unix.EIO)'
        run_overlay client-connect-refused "$linux_test" '^TestVsockLoopback$' 1 '--- FAIL: TestVsockLoopback/single_write' \
            'connection refused' 'if err := unix.Connect(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_LOCAL, Port: addr.Port}); err != nil {' \
            'if err := error(unix.ECONNREFUSED); err != nil {'
        grep -F -- 'connect vsock loopback client to VMADDR_CID_LOCAL port' "$logs/client-connect-refused.log"
        ;;
    darwin)
        run_positive '^(TestSocketCloexec|TestCreateCloexecMarkFailureClosesDescriptor)$' \
            TestSocketCloexec TestCreateCloexecMarkFailureClosesDescriptor
        run_overlay descriptor-mark-io-error "$unix_test" '^TestSocketCloexec$' 1 '--- FAIL: TestSocketCloexec' \
            'socketCloexec: guestlisten: mark close-on-exec: input/output error' 'fd, err := socketCloexec()' \
            'originalMark := setCloseOnExec; t.Cleanup(func() { setCloseOnExec = originalMark }); setCloseOnExec = func(int) error { return unix.EIO }; fd, err := socketCloexec()'
        ;;
    *)
        printf 'Expected linux or darwin\n' >&2
        exit 1
        ;;
esac

printf 'Native fixture and injection checks passed on %s.\n' "$platform"
