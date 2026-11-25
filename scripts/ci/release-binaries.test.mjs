// SPDX-License-Identifier: Apache-2.0
//
// Fixtures for `scripts/ci/release-binaries.sh`, the binary release steps of
// `.github/workflows/deploy.yml`, against stubbed `go`, `gh`, `curl`, macOS
// signing tools (`security`, `codesign`, `plutil`, `ditto`, `xcrun`) and a
// stubbed release manifest tool.
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import {
  chmodSync,
  copyFileSync,
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
function readBashVersion(bash) {
  return (
    spawnSync(bash, ["-c", 'echo "${BASH_VERSINFO[0]}.${BASH_VERSINFO[1]}"'], {
      encoding: "utf8",
    }).stdout?.trim() ?? ""
  );
}

const bashVersion = readBashVersion("bash");
const [bashMajor, bashMinor] = bashVersion.split(".").map(Number);
const bash32 =
  bashVersion === "3.2"
    ? "bash"
    : ["/bin/bash", "bash3.2"].find((bash) => readBashVersion(bash) === "3.2");
const macBash = bash32 ?? "bash";
const hasJq = spawnSync("sh", ["-c", "command -v jq"]).status === 0;
const dependencySkip =
  process.platform === "win32"
    ? "the release scripts run on Linux and macOS"
    : !hasJq && "jq is not installed";
const skip =
  dependencySkip ||
  (!(bashMajor > 4 || (bashMajor === 4 && bashMinor >= 4)) &&
    `the Linux release fixtures need Bash 4.4 or newer, found ${bashVersion || "none"}`);
const macSkip = dependencySkip || (!bash32 && skip);

const virtualizationEntitlement = "com.apple.security.virtualization";
const entitlementKey = `<key>${virtualizationEntitlement}</key>`;
const entitlementPlist = (body) =>
  `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict>${body}</dict></plist>`;
const enabledEntitlementPlist = entitlementPlist(`${entitlementKey}<true/>`);

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
# A GitHub Release is $STUB_DIR/release-<tag> (the \`--json isDraft,assets\`
# answer) and the asset bytes in $STUB_DIR/release-assets/<tag>/<name>.
if [[ "$1 $2" == "release view" ]]; then
  if [[ -f "$STUB_DIR/release-$3" ]]; then cat "$STUB_DIR/release-$3"; exit 0; fi
  echo "release not found" >&2
  exit 1
fi
if [[ "$1 $2" == "release download" ]]; then
  tag="$3" pattern="" out=""
  shift 3
  while (( $# > 0 )); do
    case "$1" in
      --pattern) pattern="$2"; shift ;;
      --output) out="$2"; shift ;;
    esac
    shift
  done
  cp "$STUB_DIR/release-assets/$tag/$pattern" "$out"
  exit 0
fi
if [[ "$1 $2" == "release upload" ]]; then
  tag="$3" clobber="" files=()
  shift 3
  while (( $# > 0 )); do
    case "$1" in
      --repo) shift ;;
      --clobber) clobber=1 ;;
      *) files+=("$1") ;;
    esac
    shift
  done
  # Like gh: an asset that exists is replaced only with --clobber.
  for file in "\${files[@]}"; do
    target="$STUB_DIR/release-assets/$tag/$(basename "$file")"
    if [[ -f "$target" && -z "$clobber" ]]; then
      echo "a release asset named $(basename "$file") already exists (HTTP 422)" >&2
      exit 1
    fi
    mkdir -p "$(dirname "$target")"
    cp "$file" "$target"
  done
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
if [[ "$1" == list-keychains ]]; then
  if [[ " $* " == *" -s "* ]]; then
    { printf 'security-args'; printf ' [%s]' "$@"; echo; } >> "$STUB_LOG"
  elif [[ -z "\${STUB_EMPTY_KEYCHAIN_LIST:-}" ]]; then
    printf '    "%s"\\n' "/Users/runner/Library/Keychains/login.keychain-db" "/Users/runner/Library/Keychains/other keychain.keychain-db"
  fi
fi
if [[ "$1" == find-identity ]]; then
  printf '  1) ABCDEF "%s"\\n     1 valid identities found\\n' "\${STUB_IDENTITY:-Developer ID Application: Delino (TEAM123)}"
fi
`,
  codesign: `#!/usr/bin/env bash
echo "codesign $*" >> "$STUB_LOG"
if [[ "$1" == --display ]]; then
  if [[ -n "\${STUB_ENTITLEMENT_BINARY:-}" ]]; then
    exec /usr/bin/codesign --display --entitlements - --xml "$STUB_ENTITLEMENT_BINARY"
  fi
  printf '%s\\n' "$STUB_ENTITLEMENT_PLIST"
  [[ -z "\${STUB_CODESIGN_INSPECTION_FAIL:-}" ]] || exit 1
fi
if [[ " $* " == *" --check-notarization "* && -n "\${STUB_NOT_NOTARIZED:-}" ]]; then
  echo "test-requirement: code failed to satisfy specified code requirement(s)" >&2
  exit 3
fi
`,
  plutil: `#!/usr/bin/env bash
echo "plutil $*" >> "$STUB_LOG"
[[ "$*" == "-convert json -o - -- -" ]] || exit 2
if [[ "$(uname -s)" == Darwin ]]; then
  exec /usr/bin/plutil "$@"
fi
# Match the fixture input and return the corresponding conversion result.
[[ "$(cat)" == "$STUB_ENTITLEMENT_PLIST" && -n "$STUB_ENTITLEMENT_JSON" ]] || exit 1
printf '%s\\n' "$STUB_ENTITLEMENT_JSON"
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

function fixture(t, { parent = tmpdir(), bash = "bash" } = {}) {
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
  const run = (
    args,
    env = {},
    binaries = [agent, cli],
    scriptPath = script,
  ) => {
    const result = spawnSync(bash, [scriptPath, ...args], {
      encoding: "utf8",
      cwd: root,
      env: {
        PATH: `${bin}${delimiter}${process.env.PATH}`,
        HOME: dir,
        STUB_DIR: dir,
        STUB_LOG: log,
        STUB_ENTITLEMENT_PLIST: enabledEntitlementPlist,
        STUB_ENTITLEMENT_JSON: JSON.stringify({
          [virtualizationEntitlement]: true,
        }),
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
  { skip: macSkip },
  (t) => {
    const f = fixture(t, { bash: macBash });
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
  "Bash 3.2 rejects the Linux release subcommands",
  { skip: !bash32 && "Bash 3.2 is not installed" },
  () => {
    for (const args of [
      ["build", "cross", "out"],
      ["assemble", "artifacts", "assets"],
      ["github-releases", "assets", "manifests"],
      ["fetch-manifests", "manifests"],
      ["prepare-manifests", "manifests", "assets"],
      ["sign-manifests", "manifests"],
      ["upload-manifests", "manifests"],
      ["has-manifests"],
    ]) {
      const result = spawnSync(bash32, [script, ...args], { encoding: "utf8" });
      assert.equal(result.status, 2, args.join(" "));
      assert.match(result.stderr, /needs Bash 4\.4 or newer/, args.join(" "));
    }
  },
);

test(
  "build runs the host executable to check the version",
  { skip },
  async (t) => {
    // The go stub writes an executable script that prints the version of the
    // -X value of its -ldflags (exactly, because the build compares the whole
    // output), or, for a missing value, text without the version.
    const executable =
      'printf \'#!/usr/bin/env bash\\necho "%s"\\n\' "${ldflags##*=}" > "$out"; chmod 755 "$out" ;;';
    const unversioned =
      'printf \'#!/usr/bin/env bash\\necho "%s"\\n\' "binary $GOOS-$GOARCH $CGO_ENABLED" > "$out"; chmod 755 "$out" ;;';
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
        stubs.go.replace(/printf 'binary.*"\$out" ;;/, unversioned),
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

function signedEntitlementFixture(dir, plist) {
  if (process.platform !== "darwin") return {};
  const binary = join(dir, "entitlement-fixture");
  const entitlements = join(dir, "entitlements.plist");
  copyFileSync("/usr/bin/true", binary);
  chmodSync(binary, 0o755);
  writeFileSync(entitlements, plist);
  const result = spawnSync(
    "/usr/bin/codesign",
    ["--force", "--sign", "-", "--entitlements", entitlements, binary],
    { encoding: "utf8" },
  );
  assert.equal(result.status, 0, result.stderr);
  return { STUB_ENTITLEMENT_BINARY: binary };
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
    { skip: macSkip },
    (t) => {
      let parent = tmpdir();
      if (spaced) {
        const base = mkdtempSync(join(tmpdir(), "release-binaries-parent-"));
        t.after(() => rmSync(base, { recursive: true, force: true }));
        parent = join(base, "temp with spaces");
        mkdirSync(parent);
      }
      const f = fixture(t, { parent, bash: macBash });
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
      const result = f.run(["sign-macos", input, out], {
        ...appleEnv,
        ...signedEntitlementFixture(f.dir, enabledEntitlementPlist),
      });
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
  "sign-macos restores an empty keychain search list",
  { skip: macSkip },
  (t) => {
    const f = fixture(t, { bash: macBash });
    const work = join(f.runnerTemp, "macos-signing");
    const result = f.run(
      ["sign-macos", darwinBuilds(f), join(f.dir, "signed")],
      {
        ...appleEnv,
        STUB_EMPTY_KEYCHAIN_LIST: "1",
      },
    );
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(
      result.log
        .split("\n")
        .filter((line) => line.startsWith("security-args ")),
      [
        `security-args [list-keychains] [-d] [user] [-s] [${join(work, "release.keychain-db")}]`,
        "security-args [list-keychains] [-d] [user] [-s]",
      ],
    );
    assert.match(result.log, /xcrun notarytool submit/);
    assert.match(result.log, /security delete-keychain/);
    assert.equal(existsSync(work), false);
  },
);

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
  "sign-macos rejects invalid signed entitlements before notarization",
  { skip },
  async (t) => {
    const disabled = entitlementPlist(`${entitlementKey}<false/>`);
    for (const [name, plist, converted] of [
      ["false", disabled, { [virtualizationEntitlement]: false }],
      ["missing", entitlementPlist(""), {}],
      [
        "string",
        entitlementPlist(`${entitlementKey}<string>true</string>`),
        { [virtualizationEntitlement]: "true" },
      ],
      [
        "nested",
        entitlementPlist(
          `<key>other</key><dict>${entitlementKey}<true/></dict>`,
        ),
        { other: { [virtualizationEntitlement]: true } },
      ],
      ["malformed", entitlementPlist(`${entitlementKey}<true`), null],
      [
        "inspection failed",
        enabledEntitlementPlist,
        { [virtualizationEntitlement]: true },
      ],
    ]) {
      await t.test(name, (t) => {
        const f = fixture(t);
        const result = f.run(
          ["sign-macos", darwinBuilds(f), join(f.dir, "signed")],
          {
            ...appleEnv,
            STUB_ENTITLEMENT_PLIST: plist,
            STUB_ENTITLEMENT_JSON:
              converted === null ? "" : JSON.stringify(converted),
            STUB_CODESIGN_INSPECTION_FAIL:
              name === "inspection failed" ? "1" : "",
            ...(name === "false" ? signedEntitlementFixture(f.dir, plist) : {}),
          },
        );
        assert.notEqual(result.status, 0);
        assert.match(
          result.stderr,
          /Cannot confirm an enabled com\.apple\.security\.virtualization entitlement/,
        );
        assert.doesNotMatch(result.log, /ditto|xcrun notarytool/);
      });
    }
  },
);

test(
  "sign-macos fails without a secret or exactly one identity",
  { skip },
  async (t) => {
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

// A checkout whose root holds a dot-prefixed license file, with its own copy
// of the script: the script reads the license files from the repository root
// that it sits in.
function hiddenLicenseCheckout(f) {
  const checkout = join(f.dir, "checkout");
  const copy = join(checkout, "scripts/ci/release-binaries.sh");
  write(copy, readFileSync(script), 0o755);
  write(join(checkout, "LICENSE"), "visible license\n");
  write(join(checkout, ".license"), "hidden license\n");
  const binary = { ...cli, licenseFiles: ["LICENSE", ".license"] };
  return (assets) =>
    f.run(["assemble", artifacts(f), assets], {}, [binary], copy);
}

// Prints the file names that SHA256SUMS lists, in order.
function listedNames(sums) {
  return sums
    .trim()
    .split("\n")
    .map((line) => line.replace(/^[0-9a-f]{64} {2}/, ""));
}

function sha256Check(dir) {
  return spawnSync(
    "sh",
    ["-c", "sha256sum -c SHA256SUMS || shasum -a 256 -c SHA256SUMS"],
    { cwd: dir, encoding: "utf8" },
  );
}

test(
  "assemble lists a dot-prefixed license file in SHA256SUMS exactly once",
  { skip },
  (t) => {
    const f = fixture(t);
    const assets = join(f.dir, "assets");
    const result = hiddenLicenseCheckout(f)(assets);
    assert.equal(result.status, 0, result.stderr);
    const dir = join(assets, "ownsand-cli");
    const published = readdirSync(dir)
      .filter((name) => name !== "SHA256SUMS")
      .sort();
    assert.ok(published.includes(".license"));
    assert.ok(published.includes("LICENSE"));
    const names = listedNames(readFileSync(join(dir, "SHA256SUMS"), "utf8"));
    assert.deepEqual([...names].sort(), published);
    assert.equal(names.filter((name) => name === ".license").length, 1);
    const check = sha256Check(dir);
    assert.equal(check.status, 0, check.stdout + check.stderr);
    writeFileSync(join(dir, ".license"), "changed license\n");
    assert.notEqual(sha256Check(dir).status, 0);
  },
);

test(
  "a repeated assemble into the same directory keeps SHA256SUMS verifiable and free of itself",
  { skip },
  (t) => {
    const f = fixture(t);
    const assets = join(f.dir, "assets");
    const assemble = hiddenLicenseCheckout(f);
    const dir = join(assets, "ownsand-cli");
    let previous;
    for (const run of ["first", "second", "third"]) {
      const result = assemble(assets);
      assert.equal(result.status, 0, `${run}: ${result.stderr}`);
      const sums = readFileSync(join(dir, "SHA256SUMS"), "utf8");
      const names = listedNames(sums);
      assert.ok(!names.includes("SHA256SUMS"), `${run}: lists itself`);
      assert.equal(new Set(names).size, names.length, `${run}: duplicates`);
      const check = sha256Check(dir);
      assert.equal(check.status, 0, `${run}: ${check.stdout}${check.stderr}`);
      if (previous !== undefined) assert.equal(sums, previous);
      previous = sums;
    }
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

// The manifest directory after `fetch-manifests` and `prepare-manifests`: the
// candidate manifest, and the signed current manifest when `current` lists
// versions as `{ version, artifacts: { <asset>: <bytes> } }`.
function prepared(f, current) {
  const dir = join(f.dir, "manifests");
  write(join(dir, "ownsand-agent/manifest.json"), "{}\n");
  if (current) {
    write(
      join(dir, "ownsand-agent/current.json"),
      JSON.stringify({
        schema: 1,
        releases: current.map(({ version, artifacts }) => ({
          version,
          artifacts: Object.entries(artifacts).map(([asset, bytes]) => ({
            platform: "linux-amd64",
            url: `https://github.com/owner/repo/releases/download/ownsand-agent%40${version}/${asset}`,
            sha256: sha256Of(bytes),
          })),
        })),
      }),
    );
    write(join(dir, "ownsand-agent/current.json.sig"), "sig\n");
  }
  return dir;
}

const sha256Of = (bytes) => createHash("sha256").update(bytes).digest("hex");

// An existing GitHub Release of the stub: `assets` maps names to bytes.
function remoteRelease(f, tag, { draft = false, assets = {} } = {}) {
  write(
    join(f.dir, `release-${tag}`),
    JSON.stringify({
      isDraft: draft,
      assets: Object.keys(assets).map((name) => ({ name })),
    }),
  );
  for (const [name, bytes] of Object.entries(assets)) {
    write(join(f.dir, "release-assets", tag, name), bytes);
  }
}

const releaseWrites = /^gh release (create|upload|edit|delete) /m;

test(
  "github-releases creates a release for each checked tag",
  { skip },
  (t) => {
    const f = fixture(t);
    tagApi(f, "ownsand-agent@0.1.0");
    tagApi(f, "ownsand-cli@0.0.1");
    const result = f.run(["github-releases", assembled(f), prepared(f)]);
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
  "github-releases completes an unfinished draft of an unlisted version",
  { skip },
  (t) => {
    const f = fixture(t);
    tagApi(f, "ownsand-agent@0.1.0");
    // The draft holds stale bytes and no signed manifest lists the version, so
    // nothing references the asset yet: the assets of this run replace it.
    remoteRelease(f, "ownsand-agent@0.1.0", {
      draft: true,
      assets: { "ownsand-agent-linux-amd64": "stale\n" },
    });
    const manifests = prepared(f, [
      { version: "0.0.9", artifacts: { "ownsand-agent-linux-amd64": "old\n" } },
    ]);
    const result = f.run(["github-releases", assembled(f), manifests], {}, [
      agent,
    ]);
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
  "github-releases keeps the assets of a version that the signed manifest lists",
  { skip },
  async (t) => {
    const listedAssets = { "ownsand-agent-linux-amd64": "a\n" };
    const manifestOf = (f) =>
      prepared(f, [{ version: "0.1.0", artifacts: listedAssets }]);
    await t.test("identical retry reuses every asset", (t) => {
      const f = fixture(t);
      tagApi(f, "ownsand-agent@0.1.0");
      remoteRelease(f, "ownsand-agent@0.1.0", {
        assets: { ...listedAssets, SHA256SUMS: "sums\n" },
      });
      const result = f.run(
        ["github-releases", assembled(f), manifestOf(f)],
        {},
        [agent],
      );
      assert.equal(result.status, 0, result.stderr);
      assert.doesNotMatch(result.log, releaseWrites);
      assert.doesNotMatch(result.log, /--clobber/);
      assert.match(result.stdout, /keeping its existing assets/);
    });
    await t.test(
      "an unfinished draft is published without a re-upload",
      (t) => {
        const f = fixture(t);
        tagApi(f, "ownsand-agent@0.1.0");
        remoteRelease(f, "ownsand-agent@0.1.0", {
          draft: true,
          assets: { ...listedAssets, SHA256SUMS: "sums\n" },
        });
        const result = f.run(
          ["github-releases", assembled(f), manifestOf(f)],
          {},
          [agent],
        );
        assert.equal(result.status, 0, result.stderr);
        assert.doesNotMatch(result.log, /gh release (create|upload|delete)/);
        assert.match(
          result.log,
          /gh release edit ownsand-agent@0\.1\.0 .*--draft=false/,
        );
      },
    );
    await t.test("a missing asset is restored without overwriting", (t) => {
      const f = fixture(t);
      tagApi(f, "ownsand-agent@0.1.0");
      remoteRelease(f, "ownsand-agent@0.1.0", {
        assets: { SHA256SUMS: "sums\n" },
      });
      const result = f.run(
        ["github-releases", assembled(f), manifestOf(f)],
        {},
        [agent],
      );
      assert.equal(result.status, 0, result.stderr);
      const uploads = result.log.match(/^gh release upload .*$/gm) ?? [];
      assert.equal(uploads.length, 1);
      assert.match(uploads[0], /ownsand-agent-linux-amd64$/);
      assert.doesNotMatch(uploads[0], /--clobber|SHA256SUMS/);
      assert.doesNotMatch(result.log, /gh release (create|edit|delete)/);
      assert.equal(
        readFileSync(
          join(f.dir, "release-assets/ownsand-agent@0.1.0/SHA256SUMS"),
          "utf8",
        ),
        "sums\n",
      );
    });
    await t.test("a deleted release is recreated from matching bytes", (t) => {
      const f = fixture(t);
      tagApi(f, "ownsand-agent@0.1.0");
      const result = f.run(
        ["github-releases", assembled(f), manifestOf(f)],
        {},
        [agent],
      );
      assert.equal(result.status, 0, result.stderr);
      assert.match(result.log, /gh release create ownsand-agent@0\.1\.0 /);
      assert.doesNotMatch(result.log, /--clobber/);
    });
  },
);

test(
  "github-releases fails before any change when bytes differ from the signed manifest",
  { skip },
  async (t) => {
    // The CLI comes first in the plan: its release must not be created before
    // the checks of the agent that follows it have passed.
    const plan = [cli, agent];
    await t.test("staged bytes differ", (t) => {
      const f = fixture(t);
      tagApi(f, "ownsand-agent@0.1.0");
      tagApi(f, "ownsand-cli@0.0.1");
      const manifests = prepared(f, [
        {
          version: "0.1.0",
          artifacts: { "ownsand-agent-linux-amd64": "signed\n" },
        },
      ]);
      const result = f.run(
        ["github-releases", assembled(f), manifests],
        {},
        plan,
      );
      assert.notEqual(result.status, 0);
      assert.match(
        result.stderr,
        /The staged ownsand-agent-linux-amd64 of ownsand-agent@0\.1\.0 differs from the SHA-256 that the signed manifest records/,
      );
      assert.doesNotMatch(result.log, releaseWrites);
    });
    await t.test("an existing asset has other bytes", (t) => {
      const f = fixture(t);
      tagApi(f, "ownsand-agent@0.1.0");
      tagApi(f, "ownsand-cli@0.0.1");
      remoteRelease(f, "ownsand-agent@0.1.0", {
        assets: { "ownsand-agent-linux-amd64": "tampered\n" },
      });
      const manifests = prepared(f, [
        { version: "0.1.0", artifacts: { "ownsand-agent-linux-amd64": "a\n" } },
      ]);
      const result = f.run(
        ["github-releases", assembled(f), manifests],
        {},
        plan,
      );
      assert.notEqual(result.status, 0);
      assert.match(
        result.stderr,
        /The existing ownsand-agent-linux-amd64 of ownsand-agent@0\.1\.0 does not match the SHA-256 that the signed manifest records/,
      );
      assert.doesNotMatch(result.log, releaseWrites);
      assert.equal(
        readFileSync(
          join(
            f.dir,
            "release-assets/ownsand-agent@0.1.0/ownsand-agent-linux-amd64",
          ),
          "utf8",
        ),
        "tampered\n",
      );
    });
    await t.test("the manifest lists an asset that was not staged", (t) => {
      const f = fixture(t);
      tagApi(f, "ownsand-agent@0.1.0");
      tagApi(f, "ownsand-cli@0.0.1");
      const manifests = prepared(f, [
        {
          version: "0.1.0",
          artifacts: { "ownsand-agent-darwin-arm64": "d\n" },
        },
      ]);
      const result = f.run(
        ["github-releases", assembled(f), manifests],
        {},
        plan,
      );
      assert.notEqual(result.status, 0);
      assert.match(
        result.stderr,
        /lists ownsand-agent-darwin-arm64 for ownsand-agent@0\.1\.0, but this run staged no such asset/,
      );
      assert.doesNotMatch(result.log, releaseWrites);
    });
    await t.test("prepare-manifests did not run", (t) => {
      const f = fixture(t);
      tagApi(f, "ownsand-agent@0.1.0");
      tagApi(f, "ownsand-cli@0.0.1");
      const result = f.run(
        ["github-releases", assembled(f), join(f.dir, "manifests")],
        {},
        plan,
      );
      assert.notEqual(result.status, 0);
      assert.match(
        result.stderr,
        /manifest\.json is missing; run fetch-manifests and prepare-manifests first/,
      );
      assert.doesNotMatch(result.log, releaseWrites);
    });
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
        const result = f.run(
          ["github-releases", assembled(f), prepared(f)],
          {},
          [agent],
        );
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

// The steps of the `publish-binaries` job of `deploy.yml`, as text.
function publishSteps() {
  const workflow = readFileSync(
    join(root, ".github/workflows/deploy.yml"),
    "utf8",
  );
  const job =
    workflow.match(/^ {2}publish-binaries:\n(?:(?: {4}.*)?\n)+/m)?.[0] ?? "";
  return job.split(/^ {6}- /m).slice(1);
}

test("publish-binaries checks the signed manifest before it changes a release", () => {
  const steps = publishSteps();
  const index = (needle) => steps.findIndex((step) => step.includes(needle));
  const order = [
    "release-binaries.sh assemble",
    "release-binaries.sh fetch-manifests",
    "release-binaries.sh prepare-manifests",
    "release-binaries.sh sign-manifests",
    "release-binaries.sh github-releases",
    "actions/upload-artifact@",
    "release-binaries.sh upload-manifests",
  ].map(index);
  assert.ok(
    order.every((position) => position >= 0),
    order.join(),
  );
  assert.deepEqual(
    order,
    [...order].sort((a, b) => a - b),
  );
  // The release step reads the prepared manifest directory.
  assert.match(
    steps[order[4]],
    /github-releases "\$RUNNER_TEMP\/assets" "\$RUNNER_TEMP\/manifests"/,
  );
});

test("publish-binaries stores the signed manifests before the first R2 upload", () => {
  const steps = publishSteps();
  const index = (needle) => steps.findIndex((step) => step.includes(needle));
  const saveAt = index("actions/upload-artifact@");
  const save = steps[saveAt];
  const sign = index("release-binaries.sh sign-manifests");
  const upload = index("release-binaries.sh upload-manifests");
  assert.ok(sign >= 0 && sign < saveAt);
  assert.ok(saveAt < upload);
  // Only `upload-manifests` writes to the bucket.
  assert.equal(
    steps.filter((step) => step.includes("upload-manifests")).length,
    1,
  );
  // A failed save stops the job: no `always()`, no ignored missing files.
  assert.doesNotMatch(save, /always\(\)|continue-on-error/);
  assert.match(save, /^ {10}if-no-files-found: error$/m);
  assert.doesNotMatch(save, /if-no-files-found: (ignore|warn)/);
  // Only a release with an update manifest requires the artifact: a CLI-only
  // release has no manifests and skips the step.
  assert.match(
    save,
    /^ {8}if: steps\.manifests\.outputs\.has_manifests == 'true'$/m,
  );
  const checkAt = index("release-binaries.sh has-manifests");
  assert.ok(checkAt >= 0 && checkAt < sign);
  assert.match(steps[checkAt], /^ {8}id: manifests$/m);
});

test(
  "has-manifests reports whether a release has an update manifest",
  { skip },
  (t) => {
    const f = fixture(t);
    const output = join(f.dir, "github-output");
    for (const [binaries, expected] of [
      [[agent, cli], "has_manifests=true\n"],
      [[cli], "has_manifests=false\n"],
    ]) {
      writeFileSync(output, "");
      const result = f.run(
        ["has-manifests"],
        { GITHUB_OUTPUT: output },
        binaries,
      );
      assert.equal(result.status, 0, result.stderr);
      assert.equal(readFileSync(output, "utf8"), expected);
    }
    // A malformed plan fails instead of reading as "no manifest".
    const result = f.run(["has-manifests"], { GITHUB_OUTPUT: output }, []);
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /BINARIES is not a nonempty list/);
  },
);

test("publish-binaries saves the manifests under an attempt-specific name", () => {
  const steps = publishSteps();
  const saves = steps.filter((step) =>
    step.includes("actions/upload-artifact@"),
  );
  assert.equal(saves.length, 1);
  // A rerun keeps the run ID, so a fixed name would collide with the artifact
  // of the earlier attempt.
  assert.match(
    saves[0],
    /^ {10}name: release-manifests-\$\{\{ github\.run_attempt \}\}$/m,
  );
  assert.match(saves[0], /^ {10}retention-days: 30$/m);
  assert.doesNotMatch(saves[0], /^ {10}name: release-manifests$/m);
});

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
      // `github-releases` needs the manifest directory as well.
      ["github-releases", "assets"],
      ["publish", "x"],
    ]) {
      const result = f.run(args);
      assert.equal(result.status, 2, `${args}: ${result.stderr}`);
      assert.match(result.stderr, /^usage: /);
    }
  });
});
