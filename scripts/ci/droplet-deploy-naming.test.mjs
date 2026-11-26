// SPDX-License-Identifier: Apache-2.0
//
// Fixtures for the container name and network alias that `droplet-deploy.sh`
// gives to every replacement. Unlike the stub in `deploy-scripts.test.mjs`,
// the `docker` stub here keeps the containers between invocations and rejects
// a duplicate container name like Docker does, so a test can run deployments
// one after another within the same clock second.
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import {
  chmodSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { delimiter, join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { deadlineSkipReason } from "./fixture-deadline.mjs";

const root = fileURLToPath(new URL("../..", import.meta.url));
const dropletDeploy = join(root, "scripts/ci/droplet-deploy.sh");
const script = readFileSync(dropletDeploy, "utf8");

// The script needs Bash 4.4 or newer and util-linux `flock`; macOS ships
// Bash 3.2, and Windows has neither.
const bashVersion = spawnSync(
  "bash",
  ["-c", 'echo "${BASH_VERSINFO[0]} ${BASH_VERSINFO[1]}"'],
  { encoding: "utf8" },
)
  .stdout?.trim()
  .split(" ")
  .map(Number) ?? [0, 0];
const hasFlock = spawnSync("sh", ["-c", "command -v flock"]).status === 0;
function skipReason() {
  if (process.platform === "win32") return "the deploy scripts run on Linux";
  if (!hasFlock) return "flock is not installed";
  if (spawnSync("sh", ["-c", "command -v python3"]).status !== 0) {
    return "python3 is not installed";
  }
  if (bashVersion[0] < 4 || (bashVersion[0] === 4 && bashVersion[1] < 4)) {
    return `the deploy scripts need Bash 4.4 or newer, found ${bashVersion.join(".")}`;
  }
  return deadlineSkipReason();
}
const skip = skipReason();

const digestImage = (char) =>
  `registry.digitalocean.com/delino/api@sha256:${char.repeat(64)}`;
const image = digestImage("e");
const olderImage = digestImage("c");
const oldestImage = digestImage("b");
const frozenSecond = "20261122010101";
const uuidA = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b";
const uuidB = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5c";
const compact = (uuid) => uuid.replaceAll("-", "");
const udpPorts = "3478,5349";
const dnsLabel = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

function scratch(t) {
  const dir = mkdtempSync(join(tmpdir(), "droplet-naming-"));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  return dir;
}

function shellStub(bin, name, body) {
  const file = join(bin, name);
  writeFileSync(
    file,
    `#!/usr/bin/env bash\necho "${name} $*" >> "$STUB_DIR/calls.log"\n${body}\n`,
  );
  chmodSync(file, 0o755);
}

// `docker` stub that keeps one line per container in `containers`:
// `<id> <name> <service label> <image>`. It supports exactly the commands that
// the deploy script runs. `docker run` and `docker create` fail with the
// message of the Docker daemon when the name is taken, and nothing else
// changes. `STUB_RM_FAIL` lists the targets that a plain `docker rm` fails for.
const dockerBody = `
containers="$STUB_DIR/containers"
touch "$containers"
find_by() { # <name or id> -> prints the matching line
  awk -v key="$1" '$1 == key || $2 == key' "$containers"
}
case "$1" in
  ps)
    if [[ "$*" == *"--filter name="* ]]; then
      all="$*"; name="\${all#*--filter name=^/}"; name="\${name%%\\$*}"
      awk -v key="$name" '$2 == key { print $1 }' "$containers"
    else
      all="$*"; service="\${all#*--filter label=io.delino.service=}"; service="\${service%% *}"
      awk -v key="$service" '$3 == key { print $1 }' "$containers"
    fi ;;
  inspect)
    case "$3" in
      *State.Status*) echo running ;;
      *Config.Image*) find_by "$4" | awk '{ print $4 }' ;;
      *) echo 172.18.0.5 ;;
    esac ;;
  run|create)
    name=""; service=""; args=("$@")
    for ((i = 0; i < \${#args[@]}; i++)); do
      case "\${args[i]}" in
        --name) name="\${args[i+1]}" ;;
        io.delino.service=*) service="\${args[i]#io.delino.service=}" ;;
      esac
    done
    if [[ -n "$(find_by "$name")" ]]; then
      echo "docker: Error response from daemon: Conflict. The container name \\"/$name\\" is already in use." >&2
      exit 125
    fi
    count=$(( $(cat "$STUB_DIR/counter" 2> /dev/null || echo 0) + 1 ))
    echo "$count" > "$STUB_DIR/counter"
    echo "new$count $name $service \${args[\${#args[@]}-1]}" >> "$containers"
    echo "new$count" ;;
  exec)
    if [[ "$2" == -i ]]; then
      cat > /dev/null
      printf '%s' 200
    elif [[ "$3" == caddy && "$4" == adapt ]]; then
      awk '/^[a-z0-9.-]+ [{]$/ { host = $1 } /reverse_proxy / { print host, $2 }' "$DROPLET_ROOT"/caddy/sites/*.caddy |
        python3 -c '
import json
import sys
routes = []
for line in sys.stdin:
    host, dial = line.split()
    proxy = {"handler": "reverse_proxy", "upstreams": [{"dial": dial}]}
    subroute = {"handler": "subroute", "routes": [{"handle": [proxy]}]}
    routes.append({"match": [{"host": [host]}], "handle": [subroute]})
print(json.dumps({"apps": {"http": {"servers": {"srv0": {"routes": routes}}}}}))
'
    else
      printf '%s' '"/config/ abcd"'
    fi ;;
  rm)
    force=""; target="$2"
    if [[ "$2" == --force ]]; then force=1; target="$3"; fi
    [[ -n "$force" || " \${STUB_RM_FAIL:-} " != *" $target "* ]] || exit 1
    line="$(find_by "$target")"
    if [[ -z "$line" ]]; then [[ -n "$force" ]] || exit 1; exit 0; fi
    awk -v key="$target" '!($1 == key || $2 == key)' "$containers" > "$containers.tmp"
    mv "$containers.tmp" "$containers" ;;
  logs) echo "server log line" ;;
esac`;

// Prepares a Droplet root with the env files of `services` and an optional
// serving container, route and state for `api`.
function setup(t, { services = ["api"], serving = false, route, state } = {}) {
  const dir = scratch(t);
  const bin = join(dir, "bin");
  const deployRoot = join(dir, "srv");
  mkdirSync(bin);
  mkdirSync(join(deployRoot, "env"), { recursive: true });
  mkdirSync(join(deployRoot, "caddy", "sites"), { recursive: true });
  mkdirSync(join(deployRoot, "state"), { recursive: true });
  writeFileSync(join(dir, "calls.log"), "");
  writeFileSync(join(dir, "containers"), "");
  for (const service of services) {
    writeFileSync(
      join(deployRoot, "env", `${service}.env`),
      "LOG_LEVEL=info\n",
    );
  }
  if (serving) {
    writeFileSync(
      join(dir, "containers"),
      `old1 api-serving api ${olderImage}\n`,
    );
  }
  if (route !== undefined) {
    writeFileSync(join(deployRoot, "caddy", "sites", "api.caddy"), route);
  }
  if (state !== undefined) {
    writeFileSync(join(deployRoot, "state", "api.current"), state);
  }
  shellStub(bin, "docker", dockerBody);
  shellStub(bin, "curl", 'printf "%s" 200');
  shellStub(bin, "date", `echo ${frozenSecond}`);
  const read = (path) => {
    try {
      return readFileSync(join(deployRoot, path), "utf8");
    } catch {
      return undefined;
    }
  };
  const containers = () =>
    readFileSync(join(dir, "containers"), "utf8")
      .trim()
      .split("\n")
      .filter(Boolean)
      .map((line) => {
        const [id, name, service, containerImage] = line.split(" ");
        return { id, name, service, image: containerImage };
      });
  const calls = () =>
    readFileSync(join(dir, "calls.log"), "utf8").trim().split("\n");
  return { dir, bin, deployRoot, read, containers, calls };
}

// Runs the deploy script on its standard input, as `deploy-targets.sh` does.
// `uuid` is written to the identifier source file; `idSource` replaces its
// path, and `null` leaves the default (the kernel) in place.
function deploy(
  s,
  {
    service = "api",
    deployImage = image,
    udp = "none",
    uuid,
    idSource,
    env,
  } = {},
) {
  const source = join(s.dir, "id-source");
  if (uuid !== undefined) writeFileSync(source, `${uuid}\n`);
  const spawnEnv = {
    ...process.env,
    PATH: `${s.bin}${delimiter}${process.env.PATH}`,
    STUB_DIR: s.dir,
    DROPLET_ROOT: s.deployRoot,
    DROPLET_HEALTH_TIMEOUT: "3",
    ...env,
  };
  if (idSource === null) {
    delete spawnEnv.DROPLET_ID_SOURCE;
  } else {
    spawnEnv.DROPLET_ID_SOURCE = idSource ?? source;
  }
  const result = spawnSync(
    "bash",
    [
      "-s",
      "--",
      service,
      deployImage,
      "8080",
      `${service}.example.com`,
      "http1",
      udp,
    ],
    { encoding: "utf8", input: script, env: spawnEnv },
  );
  return { ...result, output: result.stdout + result.stderr };
}

const withCalls = (s, prefix) =>
  s.calls().filter((call) => call.startsWith(prefix));

for (const [label, udp, createCommand] of [
  ["docker run", "none", "docker run"],
  ["docker create (UDP ports)", udpPorts, "docker create"],
]) {
  test(
    `a retry within the same second gets its own name and alias and reaches the cleanup with ${label}`,
    { skip },
    (t) => {
      const s = setup(t, {
        serving: true,
        route: "# older route\n",
        state: `${olderImage}\n`,
      });
      writeFileSync(
        join(s.deployRoot, "state", "api.previous"),
        `${oldestImage}\n`,
      );
      // The first run switches the route and records the state, and then
      // fails to remove the old container.
      const first = deploy(s, {
        udp,
        uuid: uuidA,
        env: { STUB_RM_FAIL: "old1" },
      });
      assert.equal(first.status, 1, first.output);
      assert.match(first.stderr, /cleanup of the previous containers failed/);
      assert.equal(s.read("state/api.current"), `${image}\n`);
      assert.equal(s.read("state/api.previous"), `${olderImage}\n`);
      const afterFirst = s.containers();
      assert.deepEqual(
        afterFirst.map(({ name }) => name),
        ["api-serving", `api-${frozenSecond}-eeeeeeeeeeee-${compact(uuidA)}`],
      );
      // The retry at the same clock second and with the same digest.
      const second = deploy(s, { udp, uuid: uuidB });
      assert.equal(second.status, 0, second.output);
      const created = withCalls(s, createCommand);
      assert.equal(created.length, 2, created.join("\n"));
      const names = created.map((call) => call.match(/--name (\S+)/)[1]);
      const aliases = created.map(
        (call) => call.match(/--network-alias (\S+)/)[1],
      );
      assert.equal(new Set(names).size, 2, names.join(" "));
      assert.equal(new Set(aliases).size, 2, aliases.join(" "));
      assert.equal(aliases[1], `api-${compact(uuidB)}`);
      assert.equal(
        names[1],
        `api-${frozenSecond}-eeeeeeeeeeee-${compact(uuidB)}`,
      );
      assert.ok(s.read("caddy/sites/api.caddy").includes(`${aliases[1]}:8080`));
      // The retry removed both old containers and kept only its own.
      assert.deepEqual(
        s.containers().map(({ name }) => name),
        [names[1]],
      );
      // A retry keeps the image that served before the first attempt.
      assert.equal(s.read("state/api.current"), `${image}\n`);
      assert.equal(s.read("state/api.previous"), `${olderImage}\n`);
      // The image of the replacement is never removed by its own retry.
      assert.ok(
        !withCalls(s, "docker image rm").some((c) => c.includes(image)),
      );
    },
  );

  test(
    `a repeated identifier fails before any change when the name is taken with ${label}`,
    { skip },
    (t) => {
      const s = setup(t, { route: "# older route\n" });
      const first = deploy(s, { udp, uuid: uuidA });
      assert.equal(first.status, 0, first.output);
      const route = s.read("caddy/sites/api.caddy");
      const state = s.read("state/api.current");
      const serving = s.containers();
      assert.equal(serving.length, 1);
      const before = s.calls();
      // The identifier source repeats the identifier, and the clock second and
      // the digest are the same: Docker would reject the name.
      const second = deploy(s, { udp, uuid: uuidA });
      assert.equal(second.status, 1, second.output);
      assert.match(
        second.stderr,
        new RegExp(
          `a container named api-${frozenSecond}-eeeeeeeeeeee-${compact(uuidA)} already exists; nothing was changed`,
        ),
      );
      const added = s.calls().slice(before.length);
      // The run only pulled the image, read the clock and listed containers: it neither
      // created, stopped, started nor removed a container, and it never
      // reloaded the proxy.
      assert.deepEqual(
        added.map((call) => call.split(" ").slice(0, 2).join(" ")),
        ["docker pull", "docker ps", "date -u", "docker ps"],
        added.join("\n"),
      );
      assert.deepEqual(s.containers(), serving);
      assert.equal(s.read("caddy/sites/api.caddy"), route);
      assert.equal(s.read("state/api.current"), state);
    },
  );
}

test(
  "services with the same 35-character prefix get distinct aliases",
  { skip },
  (t) => {
    const prefix = "x".repeat(35);
    const services = [`${prefix}-alpha`, `${prefix}-bravo`];
    const s = setup(t, { services });
    const aliases = [];
    for (const [index, service] of services.entries()) {
      // The same clock second and the same digest suffix.
      const result = deploy(s, {
        service,
        uuid: index === 0 ? uuidA : uuidB,
      });
      assert.equal(result.status, 0, result.output);
      const create = withCalls(s, "docker run").at(-1);
      const alias = create.match(/--network-alias (\S+)/)[1];
      assert.match(alias, dnsLabel);
      assert.ok(alias.length <= 63, alias);
      assert.equal(
        s.read(`caddy/sites/${service}.caddy`).includes(`${alias}:8080`),
        true,
      );
      aliases.push(alias);
    }
    assert.notEqual(aliases[0], aliases[1]);
    assert.equal(new Set(s.containers().map(({ name }) => name)).size, 2);
  },
);

test("a failure to get the identifier changes nothing", { skip }, (t) => {
  const route = "# older route\n";
  for (const [label, options] of [
    ["missing file", { idSource: "/nonexistent/uuid" }],
    ["empty file", { uuid: "" }],
    ["not a UUID", { uuid: "not-a-uuid" }],
    ["too short", { uuid: "0190a1b2-c3d4" }],
    ["not hexadecimal", { uuid: "g".repeat(32) }],
  ]) {
    const s = setup(t, { serving: true, route, state: `${olderImage}\n` });
    const result = deploy(s, options);
    assert.equal(result.status, 1, `${label}: ${result.output}`);
    assert.match(
      result.stderr,
      /::error::(could not read the deployment identifier from .*|.* did not yield a UUID); nothing was changed\./,
      label,
    );
    // No docker command ran, so no container was stopped or removed.
    assert.deepEqual(s.calls(), [""], label);
    assert.deepEqual(
      s.containers().map(({ name }) => name),
      ["api-serving"],
      label,
    );
    assert.equal(s.read("caddy/sites/api.caddy"), route, label);
    assert.equal(s.read("state/api.current"), `${olderImage}\n`, label);
  }
});

test(
  "the kernel identifier gives every deployment its own name and alias",
  {
    skip:
      skip ||
      (spawnSync("cat", ["/proc/sys/kernel/random/uuid"]).status === 0
        ? false
        : "the kernel does not provide /proc/sys/kernel/random/uuid"),
  },
  (t) => {
    const s = setup(t);
    for (let run = 0; run < 2; run += 1) {
      const result = deploy(s, { idSource: null });
      assert.equal(result.status, 0, result.output);
    }
    const created = withCalls(s, "docker run");
    const names = created.map((call) => call.match(/--name (\S+)/)[1]);
    const aliases = created.map(
      (call) => call.match(/--network-alias (\S+)/)[1],
    );
    assert.equal(new Set(names).size, 2, names.join(" "));
    assert.equal(new Set(aliases).size, 2, aliases.join(" "));
    for (const alias of aliases) {
      assert.match(alias, /^api-[0-9a-f]{32}$/);
    }
    assert.deepEqual(
      s.containers().map(({ name }) => name),
      [names[1]],
    );
  },
);
