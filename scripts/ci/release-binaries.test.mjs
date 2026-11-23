// SPDX-License-Identifier: Apache-2.0
//
// Fixtures for `scripts/ci/release-binaries.sh`, the binary release steps of
// `.github/workflows/deploy.yml`, against stubbed `go`, `gh`, `curl`, macOS
// signing tools (`security`, `codesign`, `ditto`, `xcrun`) and a
// stubbed release manifest tool.
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import {
  chmodSync,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { delimiter, dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const root = fileURLToPath(new URL("../..", import.meta.url));
const script = join(root, "scripts/ci/release-binaries.sh");
const bashVersion = spawnSync(
  "bash",
  ["-c", 'echo "${BASH_VERSINFO[0]} ${BASH_VERSINFO[1]}"'],
  { encoding: "utf8" },
)
  .stdout?.trim()
  .split(" ")
  .map(Number) ?? [0, 0];
const hasJq = spawnSync("sh", ["-c", "command -v jq"]).status === 0;
function skipReason() {
  if (process.platform === "win32") return "the release scripts run on Linux";
  if (!hasJq) return "jq is not installed";
  if (bashVersion[0] < 4 || (bashVersion[0] === 4 && bashVersion[1] < 4)) {
    return `the release scripts need Bash 4.4 or newer, found ${bashVersion.join(".")}`;
  }
  return false;
}
const skip = skipReason();

const sha = "c".repeat(40);
const accountId = "0123456789abcdef0123456789abcdef";

const agent = {
  name: "ownsand-agent",
  dir: "cmds/ownsand-agent",
  kind: "binary",
  target: "github-release",
  version: "0.1.0",
  binary: "ownsand-agent",
  platforms: ["linux-amd64", "darwin-arm64", "windows-arm64"],
  cgoPlatforms: ["darwin-arm64"],
  versionVariable:
    "github.com/delinoio/mono/cmds/ownsand-agent/internal/version.Version",
  entitlements: "cmds/ownsand-agent/entitlements.plist",
  licenseFiles: ["cmds/ownsand-agent/LICENSE"],
  manifestObjectKey: "ownsand-agent/manifest.json",
  manifestPublicUrl: "https://releases.example.com/ownsand-agent/manifest.json",
};
const cli = {
  name: "ownsand-cli",
  dir: "cmds/ownsand",
  kind: "binary",
  target: "github-release",
  version: "0.0.1",
  binary: "ownsand",
  platforms: ["linux-arm64", "darwin-amd64"],
  cgoPlatforms: [],
  versionVariable: "",
  entitlements: "",
  licenseFiles: ["LICENSE", "NOTICE"],
  manifestObjectKey: "",
  manifestPublicUrl: "",
};

function write(path, content, mode) {
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, content);
  if (mode) chmodSync(path, mode);
}

// Every stub appends `<command> <arguments...>` to $STUB_LOG.
const stubs = {
  go: `#!/usr/bin/env bash
set -euo pipefail
echo "go $* CGO_ENABLED=\${CGO_ENABLED:-} GOOS=\${GOOS:-} GOARCH=\${GOARCH:-}" >> "$STUB_LOG"
case "$1" in
  env)
    case "$2" in
      GOROOT) echo "$STUB_DIR/goroot" ;;
      GOHOSTOS) echo "\${STUB_GOHOST%-*}" ;;
      GOHOSTARCH) echo "\${STUB_GOHOST#*-}" ;;
    esac ;;
  build)
    out="" ldflags=""
    while (( $# > 0 )); do
      case "$1" in
        -o) out="$2"; shift ;;
        -ldflags) ldflags="$2"; shift ;;
      esac
      shift
    done
    # The release manifest tool: a copy of its stub, so later steps can run it.
    if [[ "$out" == */tools/release-manifest ]]; then
      cp "$STUB_DIR/bin/release-manifest" "$out"
      exit 0
    fi
    printf 'binary %s %s %s\\n' "$GOOS-$GOARCH" "$CGO_ENABLED" "$ldflags" > "$out" ;;
  list) cat "$STUB_DIR/go-list.tsv" ;;
  *) echo "unexpected go $*" >&2; exit 1 ;;
esac
`,
  gh: `#!/usr/bin/env bash
set -euo pipefail
echo "gh $*" >> "$STUB_LOG"
if [[ "$1" == api ]]; then
  file="$STUB_DIR/gh-api/\${2//\\//_}.json"
  if [[ ! -f "$file" ]]; then echo "gh: Not Found (HTTP 404)" >&2; exit 1; fi
  jq -r "$4" "$file"
  exit 0
fi
if [[ "$1 $2" == "release view" ]]; then
  if [[ -f "$STUB_DIR/release-$3" ]]; then cat "$STUB_DIR/release-$3"; exit 0; fi
  echo "release not found" >&2
  exit 1
fi
`,
  // Serves the R2 S3 API from $STUB_DIR/r2/<bucket>/<key> and the public
  // custom domain from $STUB_DIR/public/<key>.
  curl: `#!/usr/bin/env bash
set -euo pipefail
echo "curl $*" >> "$STUB_LOG"
out="" write="" method=GET data="" url="" config=""
while (( $# > 0 )); do
  case "$1" in
    -o) out="$2"; shift ;;
    -w) write="$2"; shift ;;
    -X) method="$2"; shift ;;
    --data-binary) data="\${2#@}"; shift ;;
    --config) config="$(cat "$2")"; shift ;;
    -H) shift ;;
    --retry) shift ;;
    -*) ;;
    *) url="$1" ;;
  esac
  shift
done
if [[ "$url" == https://*.r2.cloudflarestorage.com/* ]]; then
  [[ "$config" == *'user = "key-id:secret-key"'* && "$config" == *'aws-sigv4 = "aws:amz:auto:s3"'* ]] || { echo "no R2 credentials" >&2; exit 1; }
  path="$STUB_DIR/r2/\${url#https://*.r2.cloudflarestorage.com/}"
  if [[ -f "$STUB_DIR/r2-status" ]]; then printf '%s' "$(cat "$STUB_DIR/r2-status")"; exit 0; fi
  if [[ "$method" == PUT ]]; then
    mkdir -p "$(dirname "$path")"; cp "$data" "$path"
    mkdir -p "$(dirname "$STUB_DIR/public/\${path#"$STUB_DIR/r2/"*/}")"
    [[ -f "$STUB_DIR/public-stale" ]] || cp "$data" "$STUB_DIR/public/\${path#"$STUB_DIR/r2/"*/}"
    exit 0
  fi
  if [[ -f "$path" ]]; then cp "$path" "$out"; printf 200; else echo NoSuchKey > "$out"; printf 404; fi
  exit 0
fi
path="$STUB_DIR/public/\${url#https://releases.example.com/}"
[[ -f "$path" ]] || exit 22
cp "$path" "$out"
`,
  security: `#!/usr/bin/env bash
echo "security $*" >> "$STUB_LOG"
if [[ "$1" == list-keychains && "$*" == *" -s "* ]]; then
  printf 'security-args'; printf ' [%s]' "$@"; echo
fi >> "$STUB_LOG"
if [[ "$1" == list-keychains && "$*" != *" -s "* ]]; then
  printf '    "%s"\\n' "/Users/runner/Library/Keychains/login.keychain-db" "/Users/runner/Library/Keychains/other keychain.keychain-db"
fi
if [[ "$1" == find-identity ]]; then
  printf '  1) ABCDEF "%s"\\n     1 valid identities found\\n' "\${STUB_IDENTITY:-Developer ID Application: Delino (TEAM123)}"
fi
`,
  codesign: `#!/usr/bin/env bash
echo "codesign $*" >> "$STUB_LOG"
if [[ "$1" == --display ]]; then
  [[ -n "\${STUB_NO_ENTITLEMENT:-}" ]] || echo '<key>com.apple.security.virtualization</key>'
fi
if [[ " $* " == *" --check-notarization "* && -n "\${STUB_NOT_NOTARIZED:-}" ]]; then
  echo "test-requirement: code failed to satisfy specified code requirement(s)" >&2
  exit 3
fi
`,
  ditto: `#!/usr/bin/env bash
echo "ditto $*" >> "$STUB_LOG"
cp "$4" "$5"
`,
  xcrun: `#!/usr/bin/env bash
echo "xcrun $*" >> "$STUB_LOG"
if [[ "$2" == submit ]]; then
  printf '{"id":"submission-1","status":"%s"}\\n' "\${STUB_NOTARY_STATUS:-Accepted}"
fi
`,
  openssl: `#!/usr/bin/env bash
echo "openssl $*" >> "$STUB_LOG"
echo 0123456789abcdef
`,
  "release-manifest": `#!/usr/bin/env bash
set -euo pipefail
echo "release-manifest $*" >> "$STUB_LOG"
case "$1" in
  check-keys) [[ -z "\${STUB_NO_KEYS:-}" ]] || { echo "no built-in key" >&2; exit 1; } ;;
  add-release)
    out=""
    for (( i = 1; i <= $#; i++ )); do [[ "\${!i}" == --out ]] && { j=$((i + 1)); out="\${!j}"; }; done
    printf '{"args":"%s"}\\n' "$*" > "$out" ;;
  sign)
    [[ -n "\${OWNSAND_AGENT_RELEASE_SIGNING_KEY:-}" ]] || exit 1
    printf 'signature\\n' > "$5" ;;
  verify) ;;
esac
`,
};

function fixture(t, parent = tmpdir()) {
  const dir = mkdtempSync(join(parent, "release-binaries-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  const bin = join(dir, "bin");
  for (const [name, content] of Object.entries(stubs)) {
    write(join(bin, name), content, 0o755);
  }
  write(join(dir, "goroot/LICENSE"), "Go license\n");
  const modules = join(dir, "modules");
  write(join(modules, "a/LICENSE"), "A license\n");
  write(join(modules, "b/COPYING.md"), "B copying\n");
  write(join(modules, "b/LICENSES/MIT.txt"), "B MIT\n");
  write(join(modules, "b/src/LICENSE"), "not a module license\n");
  write(
    join(dir, "go-list.tsv"),
    `example.com/a\tv1.0.0\t${modules}/a\n\nexample.com/b\tv2.0.0\t${modules}/b\n`,
  );
  const log = join(dir, "log");
  writeFileSync(log, "");
  const summary = join(dir, "summary");
  writeFileSync(summary, "");
  const githubEnv = join(dir, "github-env");
  writeFileSync(githubEnv, "");
  const runnerTemp = join(dir, "runner");
  mkdirSync(runnerTemp);
  const run = (args, env = {}, binaries = [agent, cli]) => {
    const result = spawnSync("bash", [script, ...args], {
      encoding: "utf8",
      cwd: root,
      env: {
        PATH: `${bin}${delimiter}${process.env.PATH}`,
        HOME: dir,
        STUB_DIR: dir,
        STUB_LOG: log,
        // No release platform, so the build does not run the stub
        // executables; the version check fixture sets a release platform.
        STUB_GOHOST: "freebsd-amd64",
        BINARIES: JSON.stringify(binaries),
        RUNNER_TEMP: runnerTemp,
        GITHUB_STEP_SUMMARY: summary,
        GITHUB_ENV: githubEnv,
        RELEASE_MANIFEST_TOOL: join(bin, "release-manifest"),
        REPOSITORY: "owner/repo",
        SHA: sha,
        GH_TOKEN: "token",
        ...env,
      },
    });
    return {
      ...result,
      log: readFileSync(log, "utf8"),
      summary: readFileSync(summary, "utf8"),
    };
  };
  return { dir, run, log, runnerTemp };
}

const r2Env = {
  RELEASES_R2_ACCOUNT_ID: accountId,
  RELEASES_R2_ACCESS_KEY_ID: "key-id",
  RELEASES_R2_SECRET_ACCESS_KEY: "secret-key",
  RELEASES_R2_BUCKET: "delino-releases",
};

const appleEnv = {
  APPLE_DEVELOPER_ID_CERTIFICATE_P12: Buffer.from("p12").toString("base64"),
  APPLE_DEVELOPER_ID_CERTIFICATE_PASSWORD: "p12-password",
  APPLE_NOTARY_API_KEY:
    "-----BEGIN PRIVATE KEY-----\nkey\n-----END PRIVATE KEY-----",
  APPLE_NOTARY_API_KEY_ID: "KEYID",
  APPLE_NOTARY_API_ISSUER_ID: "issuer",
};

test(
  "build cross builds every platform without cgo and writes the notices",
  { skip },
  (t) => {
    const f = fixture(t);
    const out = join(f.dir, "out");
    // The build job builds the manifest tool itself and checks the keys with
    // that copy, not with a tool from the environment.
    const result = f.run(["build", "cross", out], {
      RELEASE_MANIFEST_TOOL: "",
    });
    assert.equal(result.status, 0, result.stderr);
    assert.match(
      result.log,
      new RegExp(
        `^go build -trimpath -buildvcs=false -o ${out}/tools/release-manifest \\./cmds/ownsand-agent/release-manifest CGO_ENABLED=0 `,
        "m",
      ),
    );
    // The agent has an update manifest, so the build checks the built-in keys.
    assert.match(result.log, /^release-manifest check-keys$/m);
    assert.ok(
      result.log.indexOf("release-manifest check-keys") >
        result.log.indexOf("/tools/release-manifest"),
    );
    assert.equal(
      readFileSync(
        join(out, "release/ownsand-agent/ownsand-agent-linux-amd64"),
        "utf8",
      ),
      `binary linux-amd64 0 -s -w -X ${agent.versionVariable}=0.1.0\n`,
    );
    assert.equal(
      readFileSync(
        join(out, "release/ownsand-agent/ownsand-agent-windows-arm64.exe"),
        "utf8",
      ),
      `binary windows-arm64 0 -s -w -X ${agent.versionVariable}=0.1.0\n`,
    );
    // darwin-arm64 of the agent needs cgo and is left to the macOS job.
    assert.equal(existsSync(join(out, "darwin/ownsand-agent")), false);
    // The CLI has no version variable and builds darwin without cgo, into the
    // directory that the signing job reads.
    assert.equal(
      readFileSync(
        join(out, "darwin/ownsand-cli/ownsand-darwin-amd64"),
        "utf8",
      ),
      "binary darwin-amd64 0 -s -w\n",
    );
    assert.match(
      result.log,
      /go build -trimpath -buildvcs=false .* \.\/cmds\/ownsand-agent CGO_ENABLED=0 GOOS=linux GOARCH=amd64/,
    );
    // Notices: the module list of every platform, including the cgo one.
    assert.match(
      result.log,
      /go list -deps .* \.\/cmds\/ownsand-agent CGO_ENABLED=1 GOOS=darwin GOARCH=arm64/,
    );
    assert.equal(
      readFileSync(join(out, "release/ownsand-cli/GO-LICENSE"), "utf8"),
      "Go license\n",
    );
    const notices = readFileSync(
      join(out, "release/ownsand-agent/THIRD-PARTY-LICENSES.txt"),
      "utf8",
    );
    assert.match(
      notices,
      /example\.com\/a v1\.0\.0: LICENSE\n=+\n\nA license\n/,
    );
    assert.match(
      notices,
      /example\.com\/b v2\.0\.0: COPYING\.md\n=+\n\nB copying\n/,
    );
    assert.match(
      notices,
      /example\.com\/b v2\.0\.0: LICENSES\/MIT\.txt\n=+\n\nB MIT\n/,
    );
    assert.doesNotMatch(notices, /not a module license/);
    // Each module appears once although every platform lists it.
    assert.equal(
      notices.match(/example\.com\/a v1\.0\.0: LICENSE\n/g).length,
      1,
    );
    assert.match(result.summary, /ownsand-cli@0\.0\.1: `ownsand-linux-arm64`/);
  },
);

test(
  "build cross stops before any build when the agent has no built-in key",
  { skip },
  (t) => {
    const f = fixture(t);
    const result = f.run(["build", "cross", join(f.dir, "out")], {
      STUB_NO_KEYS: "1",
    });
    assert.notEqual(result.status, 0);
    // Only the manifest tool was built, no release binary.
    assert.equal(result.log.match(/^go build/gm).length, 1);
    assert.doesNotMatch(result.log, /go build .*-ldflags/);
  },
);

test(
  "build cross skips the key check without an update manifest",
  { skip },
  (t) => {
    const f = fixture(t);
    const result = f.run(
      ["build", "cross", join(f.dir, "out")],
      { STUB_NO_KEYS: "1" },
      [cli],
    );
    assert.equal(result.status, 0, result.stderr);
    assert.doesNotMatch(result.log, /check-keys/);
    // The publish job downloads the tool for every binary release.
    assert.match(result.log, /go build .*-o \S+\/tools\/release-manifest /);
  },
);

test("build fails for a module without a license file", { skip }, (t) => {
  const f = fixture(t);
  mkdirSync(join(f.dir, "modules/c"));
  writeFileSync(
    join(f.dir, "go-list.tsv"),
    `example.com/c\tv0.1.0\t${f.dir}/modules/c\n`,
  );
  const result = f.run(["build", "cross", join(f.dir, "out")], {}, [cli]);
  assert.notEqual(result.status, 0);
  assert.match(
    result.stderr,
    /Module example\.com\/c v0\.1\.0 that ownsand-cli links has no license file/,
  );
});

test(
  "build cgo builds only the cgo platforms on a matching runner",
  { skip },
  (t) => {
    const f = fixture(t);
    const out = join(f.dir, "out");
    // Without a version variable, so the build does not run the stub output.
    const binaries = [{ ...agent, versionVariable: "" }, cli];
    let result = f.run(
      ["build", "cgo", out],
      { STUB_GOHOST: "darwin-arm64" },
      binaries,
    );
    assert.equal(result.status, 0, result.stderr);
    assert.equal(
      readFileSync(
        join(out, "darwin/ownsand-agent/ownsand-agent-darwin-arm64"),
        "utf8",
      ),
      "binary darwin-arm64 1 -s -w\n",
    );
    assert.equal(result.log.match(/^go build/gm).length, 1);
    assert.doesNotMatch(result.log, /check-keys|go list/);
    result = f.run(["build", "cgo", join(f.dir, "out2")]);
    assert.notEqual(result.status, 0);
    assert.match(
      result.stderr,
      /needs a darwin-arm64 runner, not freebsd-amd64/,
    );
  },
);

test(
  "build runs the host executable to check the version",
  { skip },
  async (t) => {
    // The go stub writes an executable script that prints its own text, which
    // holds the -ldflags value.
    const executable =
      'printf \'#!/usr/bin/env bash\\necho "%s"\\n\' "binary $GOOS-$GOARCH $CGO_ENABLED $ldflags" > "$out"; chmod 755 "$out" ;;';
    await t.test("recorded", (t) => {
      const f = fixture(t);
      writeFileSync(
        join(f.dir, "bin/go"),
        stubs.go.replace(/printf 'binary.*"\$out" ;;/, executable),
      );
      const result = f.run(
        ["build", "cross", join(f.dir, "out")],
        { STUB_GOHOST: "linux-amd64" },
        [agent],
      );
      assert.equal(result.status, 0, result.stderr);
    });
    await t.test("missing", (t) => {
      const f = fixture(t);
      writeFileSync(
        join(f.dir, "bin/go"),
        stubs.go.replace(
          /printf 'binary.*"\$out" ;;/,
          executable.replace("$ldflags", ""),
        ),
      );
      const result = f.run(
        ["build", "cross", join(f.dir, "out")],
        { STUB_GOHOST: "linux-amd64" },
        [agent],
      );
      assert.notEqual(result.status, 0);
      assert.match(
        result.stderr,
        /version printed binary linux-amd64 0 ?, not the version 0\.1\.0/,
      );
    });
  },
);

function darwinBuilds(f) {
  const input = join(f.dir, "darwin");
  write(join(input, "ownsand-agent/ownsand-agent-darwin-arm64"), "agent\n");
  write(join(input, "ownsand-cli/ownsand-darwin-amd64"), "cli\n");
  return input;
}

// Escapes `value` for a literal match inside a RegExp.
function escapeRegExp(value) {
  return value.replace(/[.*+?^${}()|[\]\\/]/g, "\\$&");
}

// The scenario runs in the system temporary directory and in a directory whose
// name contains spaces, because the signing paths come from RUNNER_TEMP and the
// output directory, and each must stay one argument of the signing tools.
for (const [label, spaced] of [
  ["", false],
  [" in a temporary directory with spaces", true],
]) {
  test(
    `sign-macos signs with the hardened runtime and notarizes every darwin executable${label}`,
    { skip },
    (t) => {
      let parent = tmpdir();
      if (spaced) {
        const base = mkdtempSync(join(tmpdir(), "release-binaries-parent-"));
        t.after(() => rmSync(base, { recursive: true, force: true }));
        parent = join(base, "temp with spaces");
        mkdirSync(parent);
      }
      const f = fixture(t, parent);
      // The plain scenario inherits TMPDIR, which may itself contain spaces.
      if (spaced) assert.ok(f.dir.includes(" "));
      const input = darwinBuilds(f);
      const out = join(f.dir, "signed");
      const work = join(f.runnerTemp, "macos-signing");
      const keychain = escapeRegExp(join(work, "release.keychain-db"));
      const agentTarget = escapeRegExp(
        join(out, "ownsand-agent/ownsand-agent-darwin-arm64"),
      );
      const cliTarget = escapeRegExp(
        join(out, "ownsand-cli/ownsand-darwin-amd64"),
      );
      const result = f.run(["sign-macos", input, out], appleEnv);
      assert.equal(result.status, 0, result.stderr);
      assert.equal(
        readFileSync(
          join(out, "ownsand-agent/ownsand-agent-darwin-arm64"),
          "utf8",
        ),
        "agent\n",
      );
      assert.match(
        result.log,
        new RegExp(
          `^codesign --force --sign Developer ID Application: Delino \\(TEAM123\\) --keychain ${keychain} --options runtime --timestamp --identifier io\\.delino\\.ownsand-agent --entitlements ${escapeRegExp(root)}/?cmds/ownsand-agent/entitlements\\.plist ${agentTarget}$`,
          "m",
        ),
      );
      // The CLI has no entitlements.
      assert.match(
        result.log,
        new RegExp(
          `^codesign --force --sign Developer ID Application: Delino \\(TEAM123\\) --keychain ${keychain} --options runtime --timestamp --identifier io\\.delino\\.ownsand ${cliTarget}$`,
          "m",
        ),
      );
      assert.match(
        result.log,
        new RegExp(
          `^xcrun notarytool submit ${escapeRegExp(join(work, "ownsand-agent-darwin-arm64.zip"))} --key ${escapeRegExp(join(work, "notary-key.p8"))} --key-id KEYID --issuer issuer --wait --timeout 45m --output-format json$`,
          "m",
        ),
      );
      assert.match(
        result.log,
        new RegExp(
          `^codesign -vvvv -R=notarized --check-notarization ${agentTarget}$`,
          "m",
        ),
      );
      assert.match(
        result.log,
        new RegExp(
          `^codesign -vvvv -R=notarized --check-notarization ${cliTarget}$`,
          "m",
        ),
      );
      assert.doesNotMatch(result.log, /spctl/);
      // The password and the key are never command-line arguments of a tool
      // other than `security import`, and the keychain is deleted.
      assert.doesNotMatch(
        result.log.replace(/security import .*\n/, ""),
        /p12-password/,
      );
      assert.match(result.log, /security delete-keychain/);
      // The job keychain goes first in the search list, before signing, with
      // each existing entry as its own argument, and the saved list comes back
      // before the keychain is deleted.
      const lines = result.log.split("\n");
      const added = lines.findIndex((l) =>
        new RegExp(
          `^security list-keychains -d user -s ${keychain} /Users/runner/Library/Keychains/login\\.keychain-db /Users/runner/Library/Keychains/other keychain\\.keychain-db$`,
        ).test(l),
      );
      const firstSign = lines.findIndex((l) =>
        l.startsWith("codesign --force"),
      );
      const restored = lines.findIndex(
        (l) =>
          l ===
          "security-args [list-keychains] [-d] [user] [-s] [/Users/runner/Library/Keychains/login.keychain-db] [/Users/runner/Library/Keychains/other keychain.keychain-db]",
      );
      assert.ok(
        lines.some((l) =>
          new RegExp(
            `^security-args \\[list-keychains\\] \\[-d\\] \\[user\\] \\[-s\\] \\[${keychain}\\] \\[/Users/runner/Library/Keychains/login\\.keychain-db\\] \\[/Users/runner/Library/Keychains/other keychain\\.keychain-db\\]$`,
          ).test(l),
        ),
        "each search list entry is its own argument",
      );
      const deleted = lines.findIndex((l) =>
        l.startsWith("security delete-keychain"),
      );
      assert.ok(
        added >= 0 && added < firstSign,
        "keychain added before signing",
      );
      assert.ok(
        restored > firstSign && restored < deleted,
        "list restored before deletion",
      );
      assert.equal(existsSync(work), false);
      assert.match(result.summary, /notarization `submission-1` Accepted/);
    },
  );
}

test(
  "sign-macos fails on a rejected notarization and prints the log",
  { skip },
  (t) => {
    const f = fixture(t);
    const result = f.run(
      ["sign-macos", darwinBuilds(f), join(f.dir, "signed")],
      {
        ...appleEnv,
        STUB_NOTARY_STATUS: "Invalid",
      },
    );
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /status Invalid \(submission submission-1\)/);
    assert.match(result.log, /xcrun notarytool log submission-1/);
    assert.match(result.log, /security delete-keychain/);
  },
);

test(
  "sign-macos fails when codesign does not accept the notarization",
  { skip },
  (t) => {
    const f = fixture(t);
    const result = f.run(
      ["sign-macos", darwinBuilds(f), join(f.dir, "signed")],
      {
        ...appleEnv,
        STUB_NOT_NOTARIZED: "1",
      },
    );
    assert.notEqual(result.status, 0);
    assert.match(
      result.stderr,
      /codesign does not accept \S+ as notarized: .*failed to satisfy specified code requirement/,
    );
    assert.match(result.log, /security delete-keychain/);
  },
);

test(
  "sign-macos fails without the entitlement, a secret or exactly one identity",
  { skip },
  async (t) => {
    await t.test("entitlement", (t) => {
      const f = fixture(t);
      const result = f.run(
        ["sign-macos", darwinBuilds(f), join(f.dir, "signed")],
        {
          ...appleEnv,
          STUB_NO_ENTITLEMENT: "1",
        },
      );
      assert.notEqual(result.status, 0);
      assert.match(
        result.stderr,
        /no com\.apple\.security\.virtualization entitlement/,
      );
    });
    await t.test("secret", (t) => {
      const f = fixture(t);
      const result = f.run(
        ["sign-macos", darwinBuilds(f), join(f.dir, "signed")],
        {
          ...appleEnv,
          APPLE_NOTARY_API_KEY_ID: "",
        },
      );
      assert.notEqual(result.status, 0);
      assert.match(result.stderr, /APPLE_NOTARY_API_KEY_ID is not set/);
      assert.doesNotMatch(result.log, /codesign/);
    });
    await t.test("identity", (t) => {
      const f = fixture(t);
      const result = f.run(
        ["sign-macos", darwinBuilds(f), join(f.dir, "signed")],
        {
          ...appleEnv,
          STUB_IDENTITY: "Apple Development: Someone (TEAM123)",
        },
      );
      assert.notEqual(result.status, 0);
      assert.match(
        result.stderr,
        /exactly one Developer ID Application identity, found 0/,
      );
    });
  },
);

function artifacts(f) {
  const dir = join(f.dir, "artifacts");
  for (const [name, files] of Object.entries({
    "release/ownsand-agent": [
      "ownsand-agent-linux-amd64",
      "ownsand-agent-windows-arm64.exe",
      "GO-LICENSE",
      "THIRD-PARTY-LICENSES.txt",
    ],
    "signed/ownsand-agent": ["ownsand-agent-darwin-arm64"],
    "release/ownsand-cli": [
      "ownsand-linux-arm64",
      "GO-LICENSE",
      "THIRD-PARTY-LICENSES.txt",
    ],
    "signed/ownsand-cli": ["ownsand-darwin-amd64"],
  })) {
    for (const file of files) write(join(dir, name, file), `${file}\n`);
  }
  return dir;
}

test(
  "assemble collects the planned assets, the license files and SHA256SUMS",
  { skip },
  (t) => {
    const f = fixture(t);
    const assets = join(f.dir, "assets");
    const result = f.run(["assemble", artifacts(f), assets]);
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(readdirSync(join(assets, "ownsand-agent")).sort(), [
      "GO-LICENSE",
      "LICENSE",
      "SHA256SUMS",
      "THIRD-PARTY-LICENSES.txt",
      "ownsand-agent-darwin-arm64",
      "ownsand-agent-linux-amd64",
      "ownsand-agent-windows-arm64.exe",
    ]);
    assert.deepEqual(readdirSync(join(assets, "ownsand-cli")).sort(), [
      "GO-LICENSE",
      "LICENSE",
      "NOTICE",
      "SHA256SUMS",
      "THIRD-PARTY-LICENSES.txt",
      "ownsand-darwin-amd64",
      "ownsand-linux-arm64",
    ]);
    assert.equal(
      readFileSync(join(assets, "ownsand-agent/LICENSE"), "utf8"),
      readFileSync(join(root, "cmds/ownsand-agent/LICENSE"), "utf8"),
    );
    const sums = readFileSync(join(assets, "ownsand-cli/SHA256SUMS"), "utf8");
    assert.match(sums, /^[0-9a-f]{64} {2}ownsand-linux-arm64$/m);
    assert.equal(sums.trim().split("\n").length, 6);
  },
);

test(
  "assemble rejects missing, extra and linked artifact entries",
  { skip },
  async (t) => {
    for (const [label, change, message] of [
      [
        "missing",
        (dir) => rmSync(join(dir, "signed/ownsand-cli/ownsand-darwin-amd64")),
        /Missing or not a regular file: .*ownsand-darwin-amd64/,
      ],
      [
        "extra",
        (dir) => write(join(dir, "release/ownsand-cli/extra"), "x\n"),
        /unexpected entry: release\/ownsand-cli\/extra/,
      ],
      [
        "symlink",
        (dir) => {
          rmSync(join(dir, "release/ownsand-cli/GO-LICENSE"));
          symlinkSync(
            "/etc/passwd",
            join(dir, "release/ownsand-cli/GO-LICENSE"),
          );
        },
        /Missing or not a regular file: .*GO-LICENSE/,
      ],
    ]) {
      await t.test(label, (t) => {
        const f = fixture(t);
        const dir = artifacts(f);
        change(dir);
        const result = f.run(["assemble", dir, join(f.dir, "assets")]);
        assert.notEqual(result.status, 0);
        assert.match(result.stderr, message);
      });
    }
  },
);

// Writes the GitHub API answers for an annotated tag on a version commit
// whose parent is `parent`.
function tagApi(f, tag, { type = "tag", parent = sha } = {}) {
  const api = join(f.dir, "gh-api");
  const file = (path) => join(api, `${path.replaceAll("/", "_")}.json`);
  write(
    file(`repos/owner/repo/git/ref/tags/${tag}`),
    JSON.stringify({ object: { type, sha: `tagobj-${tag}` } }),
  );
  write(
    file(`repos/owner/repo/git/tags/tagobj-${tag}`),
    JSON.stringify({ object: { type: "commit", sha: `commit-${tag}` } }),
  );
  write(
    file(`repos/owner/repo/commits/commit-${tag}`),
    JSON.stringify({ parents: [{ sha: parent }] }),
  );
}

function assembled(f) {
  const assets = join(f.dir, "assets");
  write(join(assets, "ownsand-agent/ownsand-agent-linux-amd64"), "a\n");
  write(join(assets, "ownsand-agent/SHA256SUMS"), "sums\n");
  write(join(assets, "ownsand-cli/ownsand-linux-arm64"), "c\n");
  return assets;
}

test(
  "github-releases creates a release for each checked tag",
  { skip },
  (t) => {
    const f = fixture(t);
    tagApi(f, "ownsand-agent@0.1.0");
    tagApi(f, "ownsand-cli@0.0.1");
    const result = f.run(["github-releases", assembled(f)]);
    assert.equal(result.status, 0, result.stderr);
    assert.match(
      result.log,
      /gh release create ownsand-agent@0\.1\.0 --repo owner\/repo --verify-tag --title ownsand-agent@0\.1\.0 --notes .*CHANGELOG\.md.* --latest=false \S+\/SHA256SUMS \S+\/ownsand-agent-linux-amd64$/m,
    );
    assert.match(result.log, /gh release create ownsand-cli@0\.0\.1 /);
    assert.match(result.summary, /ownsand-agent%400\.1\.0\): 2 assets/);
  },
);

test(
  "github-releases completes an existing release on a rerun",
  { skip },
  (t) => {
    const f = fixture(t);
    tagApi(f, "ownsand-agent@0.1.0");
    writeFileSync(join(f.dir, "release-ownsand-agent@0.1.0"), "true\n");
    const result = f.run(["github-releases", assembled(f)], {}, [agent]);
    assert.equal(result.status, 0, result.stderr);
    assert.match(
      result.log,
      /gh release upload ownsand-agent@0\.1\.0 --repo owner\/repo --clobber /,
    );
    assert.match(
      result.log,
      /gh release edit ownsand-agent@0\.1\.0 --repo owner\/repo --draft=false/,
    );
    assert.doesNotMatch(result.log, /gh release create/);
  },
);

test(
  "github-releases rejects a missing, lightweight or foreign tag",
  { skip },
  async (t) => {
    for (const [label, setup, message] of [
      ["missing", () => {}, /Tag ownsand-agent@0\.1\.0 does not exist/],
      [
        "lightweight",
        (f) => tagApi(f, "ownsand-agent@0.1.0", { type: "commit" }),
        /is not an annotated tag/,
      ],
      [
        "foreign",
        (f) => tagApi(f, "ownsand-agent@0.1.0", { parent: "d".repeat(40) }),
        /is not on the version commit of the deployed revision/,
      ],
    ]) {
      await t.test(label, (t) => {
        const f = fixture(t);
        setup(f);
        const result = f.run(["github-releases", assembled(f)], {}, [agent]);
        assert.notEqual(result.status, 0);
        assert.match(result.stderr, message);
        assert.doesNotMatch(result.log, /gh release (create|upload)/);
      });
    }
  },
);

const objectPath = (f, key) => join(f.dir, "r2/delino-releases", key);

test(
  "fetch-manifests starts the first release and reads an existing pair",
  { skip },
  (t) => {
    const f = fixture(t);
    const dir = join(f.dir, "manifests");
    let result = f.run(["fetch-manifests", dir], r2Env);
    assert.equal(result.status, 0, result.stderr);
    assert.equal(existsSync(join(dir, "ownsand-agent/current.json")), false);
    // Only the agent has an update manifest.
    assert.equal(existsSync(join(dir, "ownsand-cli")), false);
    // The credentials go through a curl configuration, not the arguments.
    assert.doesNotMatch(result.log, /secret-key/);
    write(objectPath(f, "ownsand-agent/manifest.json"), "{}\n");
    write(objectPath(f, "ownsand-agent/manifest.json.sig"), "sig\n");
    result = f.run(["fetch-manifests", dir], r2Env);
    assert.equal(result.status, 0, result.stderr);
    assert.equal(
      readFileSync(join(dir, "ownsand-agent/current.json"), "utf8"),
      "{}\n",
    );
    assert.equal(
      readFileSync(join(dir, "ownsand-agent/current.json.sig"), "utf8"),
      "sig\n",
    );
    assert.match(
      result.log,
      new RegExp(
        `https://${accountId}\\.r2\\.cloudflarestorage\\.com/delino-releases/ownsand-agent/manifest\\.json`,
      ),
    );
  },
);

test(
  "fetch-manifests rejects a half pair, an HTTP error and bad settings",
  { skip },
  async (t) => {
    await t.test("half pair", (t) => {
      const f = fixture(t);
      write(objectPath(f, "ownsand-agent/manifest.json"), "{}\n");
      const result = f.run(["fetch-manifests", join(f.dir, "m")], r2Env);
      assert.notEqual(result.status, 0);
      assert.match(
        result.stderr,
        /has only one of ownsand-agent\/manifest\.json and ownsand-agent\/manifest\.json\.sig \(HTTP 200 and 404\)/,
      );
    });
    await t.test("HTTP error", (t) => {
      const f = fixture(t);
      writeFileSync(join(f.dir, "r2-status"), "403");
      const result = f.run(["fetch-manifests", join(f.dir, "m")], r2Env);
      assert.notEqual(result.status, 0);
      assert.match(result.stderr, /returned HTTP 403/);
    });
    await t.test("settings", (t) => {
      const f = fixture(t);
      let result = f.run(["fetch-manifests", join(f.dir, "m")], {
        ...r2Env,
        RELEASES_R2_BUCKET: "",
      });
      assert.match(result.stderr, /RELEASES_R2_BUCKET is not set/);
      result = f.run(["fetch-manifests", join(f.dir, "m")], {
        ...r2Env,
        RELEASES_R2_ACCOUNT_ID: "x/y",
      });
      assert.match(
        result.stderr,
        /is not a Cloudflare account ID or R2 bucket name/,
      );
      assert.doesNotMatch(result.log, /curl/);
    });
  },
);

test(
  "prepare-manifests adds each release with its download URL",
  { skip },
  (t) => {
    const f = fixture(t);
    const dir = join(f.dir, "manifests");
    const assets = join(f.dir, "assets");
    let result = f.run(["prepare-manifests", dir, assets]);
    assert.equal(result.status, 0, result.stderr);
    assert.match(
      result.log,
      new RegExp(
        `release-manifest add-release --version 0\\.1\\.0 --binary ownsand-agent --platforms linux-amd64,darwin-arm64,windows-arm64 --artifacts ${assets}/ownsand-agent --url-prefix https://github\\.com/owner/repo/releases/download/ownsand-agent%400\\.1\\.0 --out ${dir}/ownsand-agent/manifest\\.json$`,
        "m",
      ),
    );
    write(join(dir, "ownsand-agent/current.json"), "{}\n");
    result = f.run(["prepare-manifests", dir, assets]);
    assert.match(
      result.log,
      /--current \S+\/current\.json --current-signature \S+\/current\.json\.sig$/m,
    );
    assert.equal(result.log.match(/add-release/g).length, 2);
  },
);

test(
  "the manifest steps use only the prebuilt tool and never compile",
  { skip },
  async (t) => {
    const steps = (dir) => [
      [["prepare-manifests", dir, join(dir, "assets")], {}],
      [["sign-manifests", dir], { OWNSAND_AGENT_RELEASE_SIGNING_KEY: "key" }],
    ];
    for (const tool of ["", "missing", "directory", "symlink"]) {
      await t.test(tool || "unset", (t) => {
        const f = fixture(t);
        const dir = join(f.dir, "manifests");
        write(join(dir, "ownsand-agent/manifest.json"), "{}\n");
        let path = tool === "" ? "" : join(f.dir, `tool-${tool}`);
        if (tool === "directory") mkdirSync(path);
        if (tool === "symlink") {
          symlinkSync(join(f.dir, "bin/release-manifest"), path);
        }
        for (const [args, env] of steps(dir)) {
          const result = f.run(args, { ...env, RELEASE_MANIFEST_TOOL: path });
          assert.notEqual(result.status, 0, args[0]);
          assert.match(
            result.stderr,
            /RELEASE_MANIFEST_TOOL is not the release manifest tool that build cross built/,
          );
          assert.doesNotMatch(result.log, /^go /m);
        }
      });
    }
    await t.test("downloaded without its executable bit", (t) => {
      const f = fixture(t);
      const dir = join(f.dir, "manifests");
      const tool = join(f.dir, "artifacts/tools/release-manifest");
      write(tool, readFileSync(join(f.dir, "bin/release-manifest"), "utf8"));
      chmodSync(tool, 0o644);
      const result = f.run(["prepare-manifests", dir, join(f.dir, "assets")], {
        RELEASE_MANIFEST_TOOL: tool,
      });
      assert.equal(result.status, 0, result.stderr);
      assert.match(result.log, /^release-manifest add-release /m);
      assert.doesNotMatch(result.log, /^go /m);
    });
  },
);

test(
  "sign-manifests signs and verifies with the key of its step only",
  { skip },
  (t) => {
    const f = fixture(t);
    const dir = join(f.dir, "manifests");
    write(join(dir, "ownsand-agent/manifest.json"), "{}\n");
    let result = f.run(["sign-manifests", dir]);
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /OWNSAND_AGENT_RELEASE_SIGNING_KEY is not set/);
    result = f.run(["sign-manifests", dir], {
      OWNSAND_AGENT_RELEASE_SIGNING_KEY: "pem",
    });
    assert.equal(result.status, 0, result.stderr);
    assert.equal(
      readFileSync(join(dir, "ownsand-agent/manifest.json.sig"), "utf8"),
      "signature\n",
    );
    assert.match(
      result.log,
      /release-manifest verify --manifest \S+ --signature \S+manifest\.json\.sig/,
    );
  },
);

test(
  "upload-manifests uploads the pair and checks the public URL",
  { skip },
  async (t) => {
    await t.test("served", (t) => {
      const f = fixture(t);
      const dir = join(f.dir, "manifests");
      write(join(dir, "ownsand-agent/manifest.json"), '{"new":true}\n');
      write(join(dir, "ownsand-agent/manifest.json.sig"), "sig\n");
      const result = f.run(["upload-manifests", dir], r2Env);
      assert.equal(result.status, 0, result.stderr);
      assert.equal(
        readFileSync(objectPath(f, "ownsand-agent/manifest.json"), "utf8"),
        '{"new":true}\n',
      );
      assert.equal(
        readFileSync(objectPath(f, "ownsand-agent/manifest.json.sig"), "utf8"),
        "sig\n",
      );
      const puts = result.log
        .split("\n")
        .filter((line) => line.includes("-X PUT"));
      assert.equal(puts.length, 2);
      assert.match(
        puts[0],
        /Content-Type: application\/json -H Cache-Control: no-cache .*\/ownsand-agent\/manifest\.json -o \/dev\/null$/,
      );
      assert.match(
        puts[1],
        /Content-Type: text\/plain .*\/ownsand-agent\/manifest\.json\.sig -o \/dev\/null$/,
      );
      assert.match(result.summary, /serves the new manifest/);
    });
    await t.test("stale public URL", (t) => {
      const f = fixture(t);
      writeFileSync(join(f.dir, "public-stale"), "");
      const dir = join(f.dir, "manifests");
      write(join(dir, "ownsand-agent/manifest.json"), "{}\n");
      write(join(dir, "ownsand-agent/manifest.json.sig"), "sig\n");
      const result = f.run(["upload-manifests", dir], r2Env);
      assert.equal(result.status, 0, result.stderr);
      assert.match(
        result.stdout,
        /::warning::https:\/\/releases\.example\.com\/ownsand-agent\/manifest\.json does not serve the uploaded manifest yet/,
      );
    });
    await t.test("unsigned", (t) => {
      const f = fixture(t);
      const dir = join(f.dir, "manifests");
      write(join(dir, "ownsand-agent/manifest.json"), "{}\n");
      const result = f.run(["upload-manifests", dir], r2Env);
      assert.notEqual(result.status, 0);
      assert.match(result.stderr, /manifest\.json\.sig is missing/);
      assert.doesNotMatch(result.log, /-X PUT/);
    });
  },
);

test(
  "the manifest steps need no secret without an update manifest",
  { skip },
  (t) => {
    const f = fixture(t);
    const dir = join(f.dir, "manifests");
    for (const step of [
      ["fetch-manifests", dir],
      ["prepare-manifests", dir, join(f.dir, "assets")],
      ["sign-manifests", dir],
      ["upload-manifests", dir],
    ]) {
      const result = f.run(step, {}, [cli]);
      assert.equal(result.status, 0, `${step[0]}: ${result.stderr}`);
      assert.match(
        result.stdout,
        /No planned binary release has an update manifest/,
      );
    }
    assert.doesNotMatch(readFileSync(f.log, "utf8"), /curl|release-manifest/);
  },
);

test("malformed plans and arguments fail", { skip }, async (t) => {
  for (const binaries of [
    [],
    [{ ...agent, version: "1.0.0-rc.1" }],
    [{ ...agent, cgoPlatforms: ["linux-amd64"] }],
    [{ ...agent, dir: "../x" }],
  ]) {
    await t.test(JSON.stringify(binaries).slice(0, 60), (t) => {
      const f = fixture(t);
      const result = f.run(
        ["assemble", join(f.dir, "a"), join(f.dir, "b")],
        {},
        binaries,
      );
      assert.notEqual(result.status, 0);
      assert.match(
        result.stderr,
        /BINARIES is not a nonempty list of binary releases/,
      );
    });
  }
  await t.test("usage", (t) => {
    const f = fixture(t);
    for (const args of [
      [],
      ["build", "cross"],
      ["build", "other", "out"],
      ["publish", "x"],
    ]) {
      const result = f.run(args);
      assert.equal(result.status, 2, `${args}: ${result.stderr}`);
      assert.match(result.stderr, /^usage: /);
    }
  });
});
