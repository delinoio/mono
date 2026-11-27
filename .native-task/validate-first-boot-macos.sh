#!/bin/bash
set -euo pipefail

[[ "$(go env GOHOSTOS)/$(go env GOHOSTARCH)" == "darwin/arm64" ]]
output=".t3-tmp/first-boot-native"
mkdir -p "$output"
export TMPDIR="$(git rev-parse --show-toplevel)/.t3-tmp" COPYFILE_DISABLE=1
export GOOS=darwin GOARCH=arm64 CGO_ENABLED=1

package="./cmds/ownsand-agent/internal/imagebuild"
source="cmds/ownsand-agent/internal/imagebuild/macos_provision_darwin.go"
test_pattern='^TestMacOSImageFirstBootShutdown$'

go vet "$package" 2>&1 | tee "$output/vet.log"
go test "$package" -run "$test_pattern" -v -count=1 -timeout=2m 2>&1 | tee "$output/baseline.log"
[[ "$(grep -c '^    --- PASS: TestMacOSImageFirstBootShutdown/' "$output/baseline.log")" == 5 ]]

for mutation in missing-sync extra-command; do
    changed="$output/$mutation.go"
    overlay="$output/$mutation.json"
    if [[ "$mutation" == "missing-sync" ]]; then
        awk '$0 == "\t{\"/bin/sync\"}," { found++; next } { print } END { if (found != 1) exit 1 }' "$source" > "$changed"
    else
        awk '$0 == "\t{\"/bin/sync\"}," { found++; print "\t{\"/usr/bin/true\"}," } { print } END { if (found != 1) exit 1 }' "$source" > "$changed"
    fi
    printf '{"Replace":{"%s":"%s"}}\n' "$source" "$changed" > "$overlay"
    if go test -overlay "$overlay" "$package" -run "$test_pattern" -v -count=1 -timeout=2m > "$output/$mutation.log" 2>&1; then
        cat "$output/$mutation.log"
        echo "The fixture accepted $mutation."
        exit 1
    fi
    cat "$output/$mutation.log"
    [[ "$(grep -c '^    --- FAIL: TestMacOSImageFirstBootShutdown/' "$output/$mutation.log")" == 5 ]]
    [[ "$(grep -c 'guest command sequence =' "$output/$mutation.log")" == 5 ]]
done
git diff --exit-code HEAD -- "$source"
