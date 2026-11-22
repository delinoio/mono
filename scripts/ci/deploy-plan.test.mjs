// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import {
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import {
  DeployKind,
  DeployPlanError,
  DeployTarget,
  ReleaseType,
  UpstreamProtocol,
  check,
  main,
  plan,
  readWorkspacePackages,
} from "./deploy-plan.mjs";

const root = fileURLToPath(new URL("../..", import.meta.url));

const pkg = (dir, manifest) => ({ dir, manifest });

// A complete Droplet `deploy` field for the service.
const droplet = (service, fields = {}) => ({
  target: DeployTarget.Droplet,
  service,
  hostname: `${service}.example.com`,
  port: 8080,
  ...fields,
});

const workspace = [
  pkg("packages/ui", { name: "ui" }),
  pkg("servers/api", {
    name: "api",
    deploy: droplet("api-svc"),
  }),
  pkg("servers/billing", {
    name: "billing",
    dependencies: { api: "workspace:*" },
    deploy: droplet("billing"),
  }),
  pkg("apps/web", {
    name: "web",
    dependencies: { ui: "workspace:*" },
    deploy: { target: DeployTarget.CloudflarePages, project: "web" },
  }),
  pkg("apps/a-site", {
    name: "a-site",
    devDependencies: { web: "workspace:*" },
    deploy: {
      target: DeployTarget.CloudflarePages,
      project: "a-site",
      output: "doc_build",
    },
  }),
];

const status = (...releases) => ({
  changesets: [{ id: "change", releases: [], summary: "Change." }],
  releases: releases.map(([name, newVersion, type = "patch"]) => ({
    name,
    type,
    newVersion,
  })),
});

test("no pending changesets select nothing", () => {
  assert.deepEqual(plan({ changesets: [], releases: [] }, workspace), {
    changesets: [],
    releases: [],
    servers: [],
    staticSites: [],
  });
});

test("releases without a deploy field are versioned but not deployed", () => {
  const result = plan(status(["ui", "0.0.1"]), workspace);
  assert.deepEqual(result.releases, [{ name: "ui", version: "0.0.1" }]);
  assert.deepEqual(result.servers, []);
  assert.deepEqual(result.staticSites, []);
});

// Shape of `changeset status --output` for a `ui: patch` changeset when `web`
// depends on `ui` only through `devDependencies` (checked with
// @changesets/cli 3.0.3).
const devDependencyStatus = {
  changesets: [
    { releases: [{ name: "ui", type: "patch" }], summary: "Fix.", id: "fix" },
  ],
  releases: [
    {
      name: "ui",
      type: "patch",
      oldVersion: "0.0.0",
      changesets: ["fix"],
      newVersion: "0.0.1",
    },
    {
      name: "web",
      type: ReleaseType.None,
      oldVersion: "0.1.0",
      changesets: [],
      newVersion: "0.1.0",
    },
  ],
};

test("none releases are deployed but not tagged", () => {
  const result = plan(devDependencyStatus, workspace);
  assert.deepEqual(result.changesets, ["fix"]);
  assert.deepEqual(result.releases, [{ name: "ui", version: "0.0.1" }]);
  assert.deepEqual(
    result.staticSites.map((d) => [d.name, d.version]),
    [["web", "0.1.0"]],
  );
});

test("changesets with only none releases still need the release job", () => {
  const result = plan(status(["api", "0.1.0", ReleaseType.None]), workspace);
  assert.deepEqual(result.changesets, ["change"]);
  assert.deepEqual(result.releases, []);
  assert.deepEqual(
    result.servers.map((d) => d.name),
    ["api"],
  );
});

test("deployables follow workspace dependency order within each kind", () => {
  const result = plan(
    status(
      ["a-site", "1.0.1"],
      ["billing", "0.2.0"],
      ["web", "0.1.1"],
      ["api", "0.1.0"],
      ["ui", "0.0.1"],
    ),
    workspace,
  );
  assert.deepEqual(
    result.servers.map((d) => d.name),
    ["api", "billing"],
  );
  // `a-site` sorts first alphabetically but depends on `web`.
  assert.deepEqual(
    result.staticSites.map((d) => d.name),
    ["web", "a-site"],
  );
  assert.deepEqual(result.servers[0], {
    name: "api",
    dir: "servers/api",
    kind: DeployKind.Server,
    target: DeployTarget.Droplet,
    service: "api-svc",
    hostname: "api-svc.example.com",
    port: "8080",
    upstreamProtocol: UpstreamProtocol.Http1,
    udpPorts: "none",
    version: "0.1.0",
  });
  assert.equal(result.staticSites[0].output, "apps/web/dist");
  assert.equal(result.staticSites[1].output, "apps/a-site/doc_build");
  assert.deepEqual(
    result.releases.map((r) => r.name),
    ["a-site", "api", "billing", "ui", "web"],
  );
});

test("a changeset id that is not a plain file name fails", () => {
  for (const id of ["../x", "a/b", "", ".hidden"]) {
    assert.throws(
      () => plan({ changesets: [{ id }], releases: [] }, workspace),
      /not a plain file name/,
    );
  }
});

test("two deployables with the same destination fail", () => {
  const cases = [
    [
      "apps",
      { target: DeployTarget.CloudflarePages, project: "shared" },
      { target: DeployTarget.CloudflarePages, project: "shared" },
      /one and two both deploy to cloudflare-pages:shared/,
    ],
    [
      "servers",
      droplet("shared", { hostname: "one.example.com" }),
      droplet("shared", { hostname: "two.example.com" }),
      /one and two both deploy to droplet:shared/,
    ],
    [
      "servers",
      droplet("one", { hostname: "api.example.com" }),
      droplet("two", { hostname: "api.example.com" }),
      /one and two both deploy to droplet-hostname:api\.example\.com/,
    ],
    [
      "servers",
      droplet("one", { udpPorts: [3478, 3479] }),
      droplet("two", { udpPorts: [3479] }),
      /one and two both deploy to droplet-udp:3479/,
    ],
  ];
  for (const [root, first, second, message] of cases) {
    const shared = [
      pkg(`${root}/one`, { name: "one", deploy: first }),
      pkg(`${root}/two`, { name: "two", deploy: second }),
    ];
    assert.throws(() => plan(status(["one", "1.0.0"]), shared), message);
  }
});

test("a Droplet server passes its upstream protocol and UDP ports to the deploy loop", () => {
  const servers = [
    pkg("servers/one", {
      name: "one",
      deploy: droplet("one", {
        upstreamProtocol: UpstreamProtocol.H2c,
        udpPorts: [3478, 5349],
      }),
    }),
    pkg("servers/two", { name: "two", deploy: droplet("two") }),
  ];
  const result = plan(status(["one", "1.0.0"], ["two", "1.0.0"]), servers);
  assert.deepEqual(
    result.servers.map((d) => [d.name, d.upstreamProtocol, d.udpPorts]),
    [
      ["one", "h2c", "3478,5349"],
      ["two", "http1", "none"],
    ],
  );
});

test("two Droplet services may use the same container port", () => {
  const servers = [
    pkg("servers/one", { name: "one", deploy: droplet("one") }),
    pkg("servers/two", { name: "two", deploy: droplet("two") }),
  ];
  const result = plan(status(["one", "1.0.0"], ["two", "1.0.0"]), servers);
  assert.deepEqual(
    result.servers.map((d) => [d.name, d.port]),
    [
      ["one", "8080"],
      ["two", "8080"],
    ],
  );
});

test("invalid Droplet service names, host names and ports fail", () => {
  const cases = [
    [{ service: "Api" }, /deploy\.service must match/],
    [{ service: "api.v2" }, /deploy\.service must match/],
    [{ service: "-api" }, /deploy\.service must match/],
    [{ service: "api;rm" }, /deploy\.service must match/],
    [{ hostname: undefined }, /deploy\.hostname is required/],
    [{ hostname: "" }, /deploy\.hostname is required/],
    [{ hostname: "localhost" }, /deploy\.hostname must be/],
    [{ hostname: "API.example.com" }, /deploy\.hostname must be/],
    [{ hostname: "api.example.com." }, /deploy\.hostname must be/],
    [{ hostname: "*.example.com" }, /deploy\.hostname must be/],
    [{ hostname: "api example.com" }, /deploy\.hostname must be/],
    [{ hostname: ["api.example.com"] }, /deploy\.hostname is required/],
    [{ port: undefined }, /deploy\.port must be an integer from 1024 to 65535/],
    [{ port: "8080" }, /deploy\.port must be/],
    [{ port: 80 }, /deploy\.port must be/],
    [{ port: 65536 }, /deploy\.port must be/],
    [{ port: 8080.5 }, /deploy\.port must be/],
    [
      { upstreamProtocol: "h2" },
      /deploy\.upstreamProtocol must be one of http1, h2c/,
    ],
    [{ upstreamProtocol: "" }, /deploy\.upstreamProtocol must be/],
    [{ upstreamProtocol: ["h2c"] }, /deploy\.upstreamProtocol must be/],
    [{ udpPorts: [] }, /deploy\.udpPorts must be a nonempty list/],
    [{ udpPorts: 3478 }, /deploy\.udpPorts must be/],
    [{ udpPorts: ["3478"] }, /deploy\.udpPorts must be/],
    [{ udpPorts: [443] }, /deploy\.udpPorts must be/],
    [{ udpPorts: [65536] }, /deploy\.udpPorts must be/],
    [{ udpPorts: [3478.5] }, /deploy\.udpPorts must be/],
    [{ udpPorts: [3478, 3478] }, /deploy\.udpPorts must be/],
    [{ udpPorts: null }, /deploy\.udpPorts must be/],
  ];
  for (const [fields, message] of cases) {
    const deploy = droplet("api", fields);
    for (const [key, value] of Object.entries(fields)) {
      if (value === undefined) delete deploy[key];
    }
    const label = JSON.stringify(fields);
    assert.throws(
      () => plan(status(), [pkg("servers/api", { name: "api", deploy })]),
      (error) => {
        assert.ok(error instanceof DeployPlanError, label);
        assert.match(error.message, message, label);
        return true;
      },
      label,
    );
  }
  for (const fields of [
    { service: "a" },
    { service: "api-2" },
    { hostname: "a-b.c.example" },
    { port: 1024 },
    { port: 65535 },
    { upstreamProtocol: UpstreamProtocol.Http1 },
    { upstreamProtocol: UpstreamProtocol.H2c },
    { udpPorts: [1024, 65535] },
  ]) {
    assert.doesNotThrow(
      () =>
        plan(status(), [
          pkg("servers/api", { name: "api", deploy: droplet("api", fields) }),
        ]),
      JSON.stringify(fields),
    );
  }
});

test("an unknown release fails", () => {
  assert.throws(
    () => plan(status(["missing", "1.0.0"]), workspace),
    DeployPlanError,
  );
});

test("a deploy target outside its workspace root fails", () => {
  const bad = [
    pkg("apps/api", {
      name: "api",
      deploy: droplet("api"),
    }),
  ];
  assert.throws(
    () => plan(status(["api", "1.0.0"]), bad),
    /only under servers\//,
  );
});

test("a scoped deployable package name fails", () => {
  const scoped = [
    pkg("servers/api", {
      name: "@delino/api",
      deploy: droplet("api"),
    }),
  ];
  assert.throws(
    () => plan(status(["@delino/api", "1.0.0"]), scoped),
    /deployable package name/,
  );
});

test("a non-string deployable package name fails with the package directory", () => {
  const targets = [
    ["servers/api", droplet("api")],
    ["apps/web", { target: DeployTarget.CloudflarePages, project: "web" }],
  ];
  const names = [undefined, null, 123, true, ["api"]];
  for (const [dir, deploy] of targets) {
    for (const name of names) {
      const manifest = name === undefined ? { deploy } : { name, deploy };
      const label = `${dir} ${JSON.stringify(name)}`;
      assert.throws(
        () => plan(status(), [pkg(dir, manifest)]),
        (error) => {
          assert.ok(error instanceof DeployPlanError, label);
          assert.match(
            error.message,
            /a deployable package name must be a string/,
            label,
          );
          assert.ok(error.message.startsWith(`${dir}: `), label);
          assert.ok(!error.message.includes("undefined: "), label);
          return true;
        },
        label,
      );
    }
  }
});

test("string deployable names that fail the pattern still fail", () => {
  for (const name of ["", "@delino/api", "UPPER"]) {
    assert.throws(
      () =>
        plan(status(), [
          pkg("servers/api", {
            name,
            deploy: droplet("api"),
          }),
        ]),
      /a deployable package name must match/,
      JSON.stringify(name),
    );
  }
});

// A deployable of each kind with the package name, for the name rule tests.
const namedDeployables = (name) => ({
  [DeployTarget.Droplet]: pkg("servers/api", {
    name,
    deploy: droplet("api"),
  }),
  [DeployTarget.CloudflarePages]: pkg("apps/web", {
    name,
    deploy: { target: DeployTarget.CloudflarePages, project: "web" },
  }),
});

test("a deployable name with consecutive dots fails for every target", () => {
  const targets = namedDeployables("a..b");
  for (const target of Object.keys(targets)) {
    assert.throws(
      () => plan(status(), [targets[target]]),
      (error) => {
        assert.ok(error instanceof DeployPlanError, target);
        assert.match(
          error.message,
          /^a\.\.b: a deployable package name must match/,
          target,
        );
        return true;
      },
      target,
    );
  }
});

test("a droplet deployable name must be a container image repository name", () => {
  for (const name of ["a.", "a-", "a._b", "a_.b", "a___b"]) {
    assert.throws(
      () => plan(status(), [namedDeployables(name)[DeployTarget.Droplet]]),
      (error) => {
        assert.ok(error instanceof DeployPlanError, name);
        assert.ok(error.message.startsWith(`${name}: `), name);
        assert.match(error.message, /container image repository name/, name);
        return true;
      },
      name,
    );
  }
});

test("deployable names with single separators are accepted", () => {
  for (const name of ["a.b", "a_b", "a__b", "a-b", "a--b"]) {
    const targets = namedDeployables(name);
    for (const target of Object.keys(targets)) {
      assert.doesNotThrow(
        () => plan(status(), [targets[target]]),
        `${target} ${name}`,
      );
    }
  }
});

test("a static-site deployable name keeps the looser separator rule", () => {
  for (const name of ["a.", "a._b"]) {
    assert.doesNotThrow(
      () =>
        plan(status(), [namedDeployables(name)[DeployTarget.CloudflarePages]]),
      name,
    );
  }
});

test("check and the --check CLI reject a deployable name with consecutive dots", () => {
  const cli = fileURLToPath(new URL("./deploy-plan.mjs", import.meta.url));
  const cases = [
    ["servers/a..b", droplet("api")],
    ["apps/a..b", { target: DeployTarget.CloudflarePages, project: "web" }],
  ];
  for (const [dir, deploy] of cases) {
    const workDir = mkdtempSync(join(tmpdir(), "deploy-plan-dots-"));
    try {
      mkdirSync(join(workDir, ".changeset"));
      writeFileSync(join(workDir, ".changeset", "brave-cats.md"), "---\n---\n");
      mkdirSync(join(workDir, dir), { recursive: true });
      writeFileSync(
        join(workDir, dir, "package.json"),
        JSON.stringify({ name: "a..b", deploy }),
      );
      assert.throws(() => check(workDir), /a\.\.b: a deployable package name/);
      const run = spawnSync(process.execPath, [cli, "--check", workDir], {
        encoding: "utf8",
      });
      assert.equal(run.status, 2, dir);
      const event = JSON.parse(run.stderr.trim());
      assert.equal(event.event, "deploy_plan_error", dir);
      assert.match(
        event.message,
        /^a\.\.b: a deployable package name must match/,
        dir,
      );
    } finally {
      rmSync(workDir, { recursive: true, force: true });
    }
  }
});

// Names that `git check-ref-format refs/tags/<name>@<version>` rejects, plus
// `-a`, which `git tag` reads as an option.
const untaggableNames = [
  "a..b",
  "a b",
  "a~b",
  "a:b",
  "a@{b",
  "a*b",
  "a[b",
  "a\\b",
  "a?b",
  "a^b",
  "a/.b",
  "a//b",
  "a/b.lock/c",
  "-a",
  "/a",
  "a\tb",
];

const taggableNames = [
  "a.b",
  "a.",
  "a-",
  "a.lock",
  "@scope/name",
  "a/b.c/d",
  "a_b",
  "a__b",
  "0a",
];

test("a release whose name cannot form a Git tag fails", () => {
  for (const name of untaggableNames) {
    assert.throws(
      () => plan(status([name, "1.1.0"]), [pkg("packages/x", { name })]),
      (error) => {
        assert.ok(error instanceof DeployPlanError, name);
        assert.match(error.message, /must form a valid Git tag/, name);
        // The diagnostic names the package and the tag it cannot form.
        assert.ok(error.message.startsWith(`${name}: `), name);
        assert.ok(
          error.message.includes(JSON.stringify(`${name}@1.1.0`)),
          name,
        );
        return true;
      },
      name,
    );
  }
});

test("a none release whose name cannot form a Git tag fails", () => {
  assert.throws(
    () =>
      plan(status(["a..b", "1.0.0", ReleaseType.None]), [
        pkg("packages/a..b", { name: "a..b" }),
      ]),
    (error) => {
      assert.ok(error instanceof DeployPlanError);
      assert.match(
        error.message,
        /^a\.\.b: a release name must form a valid Git tag/,
      );
      return true;
    },
  );
});

test("a release whose name can form a Git tag keeps the plan fields", () => {
  for (const name of taggableNames) {
    const result = plan(status([name, "1.1.0"]), [pkg("packages/x", { name })]);
    assert.deepEqual(result.releases, [{ name, version: "1.1.0" }], name);
    assert.deepEqual(result.servers, [], name);
    assert.deepEqual(result.staticSites, [], name);
  }
});

test("a deployable that fails its own name rule keeps that diagnostic", () => {
  const targets = namedDeployables("a..b");
  for (const target of Object.keys(targets)) {
    assert.throws(
      () => plan(status(["a..b", "1.1.0"]), [targets[target]]),
      (error) => {
        assert.ok(error instanceof DeployPlanError, target);
        assert.match(
          error.message,
          /^a\.\.b: a deployable package name must match/,
          target,
        );
        return true;
      },
      target,
    );
  }
});

test("the release name rule does not apply to a package without a release", () => {
  assert.doesNotThrow(() =>
    plan(status(), [pkg("packages/a..b", { name: "a..b" })]),
  );
});

test("check and the --check CLI reject a package name that cannot form a release tag", () => {
  const cli = fileURLToPath(new URL("./deploy-plan.mjs", import.meta.url));
  const setup = (name, version) => {
    const workDir = mkdtempSync(join(tmpdir(), "deploy-plan-tag-"));
    mkdirSync(join(workDir, ".changeset"));
    writeFileSync(join(workDir, ".changeset", "brave-cats.md"), "---\n---\n");
    mkdirSync(join(workDir, "packages", name), { recursive: true });
    writeFileSync(
      join(workDir, "packages", name, "package.json"),
      JSON.stringify({ name, version, private: true }),
    );
    return workDir;
  };
  const rejected = setup("a..b", "1.0.0");
  try {
    assert.throws(
      () => check(rejected),
      (error) => {
        assert.ok(error instanceof DeployPlanError);
        assert.match(
          error.message,
          /^a\.\.b: a release name must form a valid Git tag/,
        );
        assert.ok(error.message.includes('"a..b@1.0.0"'));
        return true;
      },
    );
    const run = spawnSync(process.execPath, [cli, "--check", rejected], {
      encoding: "utf8",
    });
    assert.equal(run.status, 2);
    const event = JSON.parse(run.stderr.trim());
    assert.equal(event.event, "deploy_plan_error");
    assert.match(
      event.message,
      /^a\.\.b: a release name must form a valid Git tag/,
    );
  } finally {
    rmSync(rejected, { recursive: true, force: true });
  }
  const accepted = setup("a.b", "1.0.0");
  try {
    assert.deepEqual(check(accepted), ["brave-cats"]);
    const run = spawnSync(process.execPath, [cli, "--check", accepted], {
      encoding: "utf8",
    });
    assert.equal(run.status, 0, run.stderr);
    assert.equal(JSON.parse(run.stdout.trim()).event, "deploy_plan_check");
  } finally {
    rmSync(accepted, { recursive: true, force: true });
  }
});

test("check skips a non-deployable package whose name is not a string", () => {
  for (const manifest of [{}, { name: 123 }, { name: null }]) {
    const workDir = mkdtempSync(join(tmpdir(), "deploy-plan-noname-"));
    try {
      mkdirSync(join(workDir, "packages", "x"), { recursive: true });
      writeFileSync(
        join(workDir, "packages", "x", "package.json"),
        JSON.stringify(manifest),
      );
      assert.doesNotThrow(() => check(workDir), JSON.stringify(manifest));
    } finally {
      rmSync(workDir, { recursive: true, force: true });
    }
  }
});

test("check and the --check CLI reject a nameless deployable", () => {
  const cli = fileURLToPath(new URL("./deploy-plan.mjs", import.meta.url));
  const cases = [
    [
      "servers/api",
      {
        deploy: {
          target: "droplet",
          service: "api",
          hostname: "api.example.com",
          port: 8080,
        },
      },
    ],
    [
      "servers/api",
      {
        name: null,
        deploy: {
          target: "droplet",
          service: "api",
          hostname: "api.example.com",
          port: 8080,
        },
      },
    ],
    [
      "apps/web",
      { name: 123, deploy: { target: "cloudflare-pages", project: "web" } },
    ],
    [
      "apps/web",
      { name: true, deploy: { target: "cloudflare-pages", project: "web" } },
    ],
    [
      "servers/api",
      {
        name: ["api"],
        deploy: {
          target: "droplet",
          service: "api",
          hostname: "api.example.com",
          port: 8080,
        },
      },
    ],
  ];
  const message =
    /^(servers\/api|apps\/web): a deployable package name must be a string/;
  for (const [dir, manifest] of cases) {
    const workDir = mkdtempSync(join(tmpdir(), "deploy-plan-name-"));
    const label = JSON.stringify(manifest);
    try {
      mkdirSync(join(workDir, ".changeset"));
      writeFileSync(join(workDir, ".changeset", "brave-cats.md"), "---\n---\n");
      mkdirSync(join(workDir, dir), { recursive: true });
      writeFileSync(
        join(workDir, dir, "package.json"),
        JSON.stringify(manifest),
      );
      assert.throws(
        () => check(workDir),
        (error) => {
          assert.ok(error instanceof DeployPlanError, label);
          assert.match(error.message, message, label);
          return true;
        },
        label,
      );
      const run = spawnSync(process.execPath, [cli, "--check", workDir], {
        encoding: "utf8",
      });
      assert.equal(run.status, 2, label);
      const event = JSON.parse(run.stderr.trim());
      assert.equal(event.event, "deploy_plan_error", label);
      assert.match(event.message, message, label);
    } finally {
      rmSync(workDir, { recursive: true, force: true });
    }
  }
});

const invalidTargetMessage =
  /^(web|api): deploy\.target must be one of droplet, cloudflare-pages$/;

test("an unknown deploy target fails", () => {
  const bad = [pkg("servers/api", { name: "api", deploy: { target: "fly" } })];
  assert.throws(
    () => plan(status(["api", "1.0.0"]), bad),
    (error) => {
      assert.ok(error instanceof DeployPlanError);
      assert.match(error.message, invalidTargetMessage);
      return true;
    },
  );
});

// Every value that is not an own, valid target string must fail with the
// invalid-target diagnostic, whatever other deploy fields the manifest has.
// The values include `Object.prototype` member names, which a plain-object
// lookup would resolve to inherited members, and arrays, which a plain-object
// lookup would coerce to a valid target key.
const invalidTargets = [
  "constructor",
  "toString",
  "valueOf",
  "hasOwnProperty",
  "__proto__",
  ["droplet"],
  ["cloudflare-pages"],
  "",
  null,
  7,
  { toString: () => "droplet" },
];

test("a non-target deploy.target value fails with the invalid-target diagnostic", () => {
  for (const target of invalidTargets) {
    // `target` is a computed value, so `__proto__` stays an own data value.
    const label = JSON.stringify(target) ?? String(target);
    const deployFields = [
      { target },
      { target, project: "web" },
      { target, service: "svc" },
      { target, service: "svc", project: "web" },
    ];
    for (const deploy of deployFields) {
      for (const dir of ["apps/web", "servers/api"]) {
        const name = dir === "apps/web" ? "web" : "api";
        const bad = [pkg(dir, { name, deploy })];
        assert.throws(
          () => plan(status([name, "1.0.0"]), bad),
          (error) => {
            assert.ok(error instanceof DeployPlanError, label);
            assert.match(error.message, invalidTargetMessage, label);
            assert.doesNotMatch(
              error.message,
              /undefined\/|is required/,
              label,
            );
            return true;
          },
          label,
        );
      }
    }
  }
});

test("valid targets still plan and keep the root restriction", () => {
  const result = plan(status(["api", "1.0.0"], ["web", "1.0.0"]), workspace);
  assert.deepEqual(
    result.servers.map((d) => [d.name, d.target]),
    [["api", DeployTarget.Droplet]],
  );
  assert.deepEqual(
    result.staticSites.map((d) => [d.name, d.target]),
    [["web", DeployTarget.CloudflarePages]],
  );
  const wrongRoot = [
    pkg("apps/web", {
      name: "web",
      deploy: droplet("web"),
    }),
  ];
  assert.throws(
    () => plan(status(["web", "1.0.0"]), wrongRoot),
    /web: deploy\.target droplet is allowed only under servers\//,
  );
});

test("missing service or project names fail", () => {
  const server = [
    pkg("servers/api", {
      name: "api",
      deploy: { target: DeployTarget.Droplet },
    }),
  ];
  assert.throws(() => plan(status(["api", "1.0.0"]), server), /deploy.service/);
  const site = [
    pkg("apps/web", {
      name: "web",
      deploy: { target: DeployTarget.CloudflarePages },
    }),
  ];
  assert.throws(() => plan(status(["web", "1.0.0"]), site), /deploy.project/);
});

test("a Pages project name must be one the provider accepts", () => {
  const site = (project) => [
    pkg("apps/web", {
      name: "web",
      deploy: { target: DeployTarget.CloudflarePages, project },
    }),
  ];
  for (const project of [
    "bad name",
    "Bad-Name",
    "bad_name",
    "bad.name",
    "bad/name",
    " bad",
    "bad ",
    "-bad",
    "bad-",
  ]) {
    assert.throws(
      () => plan(status(["web", "1.0.0"]), site(project)),
      (error) => {
        assert.ok(error instanceof DeployPlanError, project);
        assert.match(error.message, /^web: deploy\.project/, project);
        return true;
      },
      project,
    );
  }
  for (const project of ["web", "a", "a-site", "site2", "a-b-c"]) {
    const result = plan(status(["web", "1.0.0"]), site(project));
    assert.equal(result.staticSites[0].project, project);
  }
});

test("check rejects an invalid Pages project name before any build", () => {
  const workDir = mkdtempSync(join(tmpdir(), "deploy-plan-project-"));
  try {
    mkdirSync(join(workDir, ".changeset"));
    writeFileSync(join(workDir, ".changeset", "brave-cats.md"), "---\n---\n");
    mkdirSync(join(workDir, "apps/web"), { recursive: true });
    writeFileSync(
      join(workDir, "apps/web", "package.json"),
      JSON.stringify({
        name: "web",
        deploy: { target: "cloudflare-pages", project: "Bad_Name" },
      }),
    );
    assert.throws(
      () => check(workDir),
      (error) => {
        assert.ok(error instanceof DeployPlanError);
        assert.match(error.message, /^web: deploy\.project/);
        return true;
      },
    );
  } finally {
    rmSync(workDir, { recursive: true, force: true });
  }
});

test("ties break alphabetically among packages that are ready", () => {
  const sites = [
    pkg("apps/a", {
      name: "a",
      dependencies: { z: "workspace:*" },
      deploy: { target: DeployTarget.CloudflarePages, project: "a" },
    }),
    pkg("apps/b", {
      name: "b",
      deploy: { target: DeployTarget.CloudflarePages, project: "b" },
    }),
    pkg("apps/z", {
      name: "z",
      deploy: { target: DeployTarget.CloudflarePages, project: "z" },
    }),
  ];
  const result = plan(
    status(["a", "1.0.0"], ["b", "1.0.0"], ["z", "1.0.0"]),
    sites,
  );
  assert.deepEqual(
    result.staticSites.map((d) => d.name),
    ["b", "z", "a"],
  );
});

test("ties break alphabetically within a stage after an earlier stage", () => {
  const mixed = [
    pkg("servers/z-api", {
      name: "z-api",
      deploy: droplet("z-api"),
    }),
    pkg("apps/a-site", {
      name: "a-site",
      dependencies: { "z-api": "workspace:*" },
      deploy: { target: DeployTarget.CloudflarePages, project: "a-site" },
    }),
    pkg("apps/b-site", {
      name: "b-site",
      deploy: { target: DeployTarget.CloudflarePages, project: "b-site" },
    }),
  ];
  const result = plan(
    status(["a-site", "1.0.0"], ["b-site", "1.0.0"], ["z-api", "1.0.0"]),
    mixed,
  );
  assert.deepEqual(
    result.servers.map((d) => d.name),
    ["z-api"],
  );
  assert.deepEqual(
    result.staticSites.map((d) => d.name),
    ["a-site", "b-site"],
  );
});

test("workspace aliases order the aliased package first", () => {
  const servers = [
    pkg("servers/a", {
      name: "a",
      dependencies: { provider: "workspace:z@*" },
      deploy: droplet("a"),
    }),
    pkg("servers/z", {
      name: "z",
      deploy: droplet("z"),
    }),
  ];
  const result = plan(status(["a", "1.0.0"], ["z", "1.0.0"]), servers);
  assert.deepEqual(
    result.servers.map((d) => d.name),
    ["z", "a"],
  );
});

test("relative workspace aliases order the linked package first", () => {
  const sites = [
    pkg("apps/accounts", {
      name: "accounts",
      dependencies: { "docs-client": "workspace:../docs" },
      deploy: { target: DeployTarget.CloudflarePages, project: "accounts" },
    }),
    pkg("apps/docs", {
      name: "docs",
      deploy: { target: DeployTarget.CloudflarePages, project: "docs" },
    }),
  ];
  const result = plan(status(["accounts", "1.0.0"], ["docs", "1.0.0"]), sites);
  assert.deepEqual(
    result.staticSites.map((d) => d.name),
    ["docs", "accounts"],
  );
});

test("only workspace: specs link workspace packages", () => {
  const servers = [
    pkg("servers/a", {
      name: "a",
      // A registry alias and a plain range under a workspace package name do
      // not link the workspace package (linkWorkspacePackages is off).
      dependencies: { b: "npm:lodash@4.17.21", c: "^1.0.0" },
      deploy: droplet("a"),
    }),
    pkg("servers/b", {
      name: "b",
      dependencies: { a: "workspace:*" },
      deploy: droplet("b"),
    }),
    pkg("servers/c", {
      name: "c",
      dependencies: { a: "workspace:^" },
      deploy: droplet("c"),
    }),
  ];
  const result = plan(
    status(["a", "1.0.0"], ["b", "1.0.0"], ["c", "1.0.0"]),
    servers,
  );
  assert.deepEqual(
    result.servers.map((d) => d.name),
    ["a", "b", "c"],
  );
});

test("dependencies through unselected packages keep their order", () => {
  const sites = [
    pkg("apps/a", {
      name: "a",
      dependencies: { mid: "workspace:*" },
      deploy: { target: DeployTarget.CloudflarePages, project: "a" },
    }),
    pkg("packages/mid", { name: "mid", dependencies: { z: "workspace:*" } }),
    pkg("apps/z", {
      name: "z",
      deploy: { target: DeployTarget.CloudflarePages, project: "z" },
    }),
  ];
  const result = plan(status(["a", "1.0.0"], ["z", "1.0.0"]), sites);
  assert.deepEqual(
    result.staticSites.map((d) => d.name),
    ["z", "a"],
  );
});

test("a static site output must be a directory inside the package", () => {
  const site = (output) => [
    pkg("apps/web", {
      name: "web",
      deploy: { target: DeployTarget.CloudflarePages, project: "web", output },
    }),
  ];
  for (const output of ["", ".", "./", "/abs", "..", "../x", "dist/../.."]) {
    assert.throws(
      () => plan(status(["web", "1.0.0"]), site(output)),
      /deploy.output/,
      JSON.stringify(output),
    );
  }
  assert.equal(
    plan(status(["web", "1.0.0"]), site("./build/")).staticSites[0].output,
    "apps/web/build",
  );
});

test("a static site output with characters the deploy loops cannot carry fails", () => {
  const site = (output) => [
    pkg("apps/web", {
      name: "web",
      deploy: { target: DeployTarget.CloudflarePages, project: "web", output },
    }),
  ];
  for (const output of [
    "a\tb",
    "a\nb",
    "a\rb",
    "a\u0007b",
    "a\u007fb",
    "a\\b",
  ]) {
    assert.throws(
      () => plan(status(["web", "1.0.0"]), site(output)),
      /web: deploy\.output must not contain/,
      JSON.stringify(output),
    );
  }
  for (const [output, expected] of [
    ["dist", "apps/web/dist"],
    ["./build/", "apps/web/build"],
    ["build/site", "apps/web/build/site"],
  ]) {
    assert.equal(
      plan(status(["web", "1.0.0"]), site(output)).staticSites[0].output,
      expected,
    );
  }
});

test("a static site output outside the package fails", () => {
  const site = [
    pkg("apps/web", {
      name: "web",
      deploy: {
        target: DeployTarget.CloudflarePages,
        project: "web",
        output: "../other",
      },
    }),
  ];
  assert.throws(() => plan(status(["web", "1.0.0"]), site), /deploy.output/);
});

test("a workspace dependency cycle fails", () => {
  const cyclic = [
    pkg("servers/a", {
      name: "a",
      dependencies: { b: "workspace:*" },
      deploy: droplet("a"),
    }),
    pkg("servers/b", {
      name: "b",
      dependencies: { a: "workspace:*" },
      deploy: droplet("b"),
    }),
  ];
  assert.throws(
    () => plan(status(["a", "1.0.0"]), cyclic),
    /dependency cycle: a -> b -> a/,
  );
});

test("deploy.needs orders a deployable after the deployables it needs", () => {
  const servers = [
    pkg("servers/a-api", {
      name: "a-api",
      deploy: droplet("a-api", { needs: ["z-accounts"] }),
    }),
    pkg("servers/b-api", { name: "b-api", deploy: droplet("b-api") }),
    pkg("servers/z-accounts", {
      name: "z-accounts",
      deploy: droplet("z-accounts"),
    }),
  ];
  const result = plan(
    status(["a-api", "1.0.0"], ["b-api", "1.0.0"], ["z-accounts", "1.0.0"]),
    servers,
  );
  assert.deepEqual(
    result.servers.map((d) => d.name),
    ["b-api", "z-accounts", "a-api"],
  );
  // The plan output does not carry the field.
  assert.equal(result.servers[2].needs, undefined);
});

test("deploy.needs orders through deployables that the run does not select", () => {
  const servers = [
    pkg("servers/a", { name: "a", deploy: droplet("a", { needs: ["m"] }) }),
    pkg("servers/m", { name: "m", deploy: droplet("m", { needs: ["z"] }) }),
    pkg("servers/z", { name: "z", deploy: droplet("z") }),
  ];
  const result = plan(status(["a", "1.0.0"], ["z", "1.0.0"]), servers);
  assert.deepEqual(
    result.servers.map((d) => d.name),
    ["z", "a"],
  );
});

test("deploy.needs does not select an unreleased deployable", () => {
  const servers = [
    pkg("servers/a", { name: "a", deploy: droplet("a", { needs: ["z"] }) }),
    pkg("servers/z", { name: "z", deploy: droplet("z") }),
  ];
  const result = plan(status(["a", "1.0.0"]), servers);
  assert.deepEqual(
    result.servers.map((d) => d.name),
    ["a"],
  );
});

test("a static site can need a server", () => {
  const mixed = [
    pkg("servers/z-api", { name: "z-api", deploy: droplet("z-api") }),
    pkg("apps/a-site", {
      name: "a-site",
      deploy: {
        target: DeployTarget.CloudflarePages,
        project: "a-site",
        needs: ["z-api"],
      },
    }),
  ];
  const result = plan(status(["a-site", "1.0.0"], ["z-api", "1.0.0"]), mixed);
  assert.deepEqual(
    result.servers.map((d) => d.name),
    ["z-api"],
  );
  assert.deepEqual(
    result.staticSites.map((d) => d.name),
    ["a-site"],
  );
});

test("invalid deploy.needs fields fail for every deployable", () => {
  const site = pkg("apps/site", {
    name: "site",
    deploy: { target: DeployTarget.CloudflarePages, project: "site" },
  });
  const cases = [
    [{ needs: "z" }, /^a: deploy\.needs must be a nonempty list/],
    [{ needs: [] }, /^a: deploy\.needs must be a nonempty list/],
    [{ needs: [""] }, /^a: deploy\.needs must be a nonempty list/],
    [{ needs: [1] }, /^a: deploy\.needs must be a nonempty list/],
    [{ needs: ["z", "z"] }, /^a: deploy\.needs must be a nonempty list/],
    [{ needs: ["a"] }, /^a: deploy\.needs must not name itself$/],
    [{ needs: ["missing"] }, /^a: deploy\.needs names missing, which is not/],
    [{ needs: ["ui"] }, /^a: deploy\.needs names ui, which is not/],
    [
      { needs: ["site"] },
      /^a: deploy\.needs names site, a static-site that deploys after every server$/,
    ],
  ];
  for (const [fields, message] of cases) {
    const packages = [
      pkg("packages/ui", { name: "ui" }),
      site,
      pkg("servers/a", { name: "a", deploy: droplet("a", fields) }),
      pkg("servers/z", { name: "z", deploy: droplet("z") }),
    ];
    // Unselected: the plan checks every deployable of the workspace.
    assert.throws(
      () => plan(status(["z", "1.0.0"]), packages),
      (error) => {
        assert.ok(error instanceof DeployPlanError);
        assert.match(error.message, message, JSON.stringify(fields));
        return true;
      },
    );
  }
});

test("a deploy.needs cycle fails even when no release selects it", () => {
  const packages = [
    pkg("servers/a", { name: "a", deploy: droplet("a", { needs: ["b"] }) }),
    pkg("servers/b", { name: "b", deploy: droplet("b", { needs: ["a"] }) }),
    pkg("servers/c", { name: "c", deploy: droplet("c") }),
  ];
  assert.throws(
    () => plan(status(["c", "1.0.0"]), packages),
    /dependency cycle: a -> b -> a/,
  );
  assert.throws(
    () => plan({ changesets: [], releases: [] }, packages),
    /dependency cycle: a -> b -> a/,
  );
});

test("repository deployables use the documented targets", () => {
  const packages = readWorkspacePackages(root);
  const all = packages.map((p) => [p.manifest.name, "1.0.0"]);
  const result = plan(status(...all), packages);
  assert.deepEqual(
    result.servers.map((d) => [
      d.name,
      d.service,
      d.hostname,
      d.port,
      d.upstreamProtocol,
      d.udpPorts,
    ]),
    [
      [
        "accounts-api",
        "accounts-api",
        "accounts.api.delino.io",
        "8080",
        "http1",
        "none",
      ],
      [
        "ownsand-api",
        "ownsand-api",
        "ownsand.api.delino.io",
        "8080",
        "h2c",
        "3478",
      ],
    ],
  );
  // The billing rollout deploys accounts-api before ownsand-api.
  assert.deepEqual(
    packages.find((p) => p.manifest.name === "ownsand-api").manifest.deploy
      .needs,
    ["accounts-api"],
  );
  assert.deepEqual(
    result.staticSites.map((d) => [d.name, d.project, d.output]),
    [
      ["delino-accounts", "delino-accounts", "apps/delino-accounts/dist"],
      ["delino-docs", "delino-docs", "apps/delino-docs/dist"],
      ["delino-site", "delino-site", "apps/delino-site/dist"],
      ["ownsand-console", "ownsand-console", "apps/ownsand/dist"],
    ],
  );
});

test("main writes GitHub outputs and a summary", () => {
  const dir = mkdtempSync(join(tmpdir(), "deploy-plan-"));
  try {
    const statusPath = join(dir, "status.json");
    const outputPath = join(dir, "output");
    const summaryPath = join(dir, "summary");
    writeFileSync(
      statusPath,
      JSON.stringify(status(["accounts-api", "0.1.0"])),
    );
    writeFileSync(outputPath, "");
    writeFileSync(summaryPath, "");
    main({
      argv: [statusPath],
      env: { GITHUB_OUTPUT: outputPath, GITHUB_STEP_SUMMARY: summaryPath },
      repoRoot: root,
    });
    const outputs = Object.fromEntries(
      readFileSync(outputPath, "utf8")
        .trim()
        .split("\n")
        .map((line) => [
          line.slice(0, line.indexOf("=")),
          line.slice(line.indexOf("=") + 1),
        ]),
    );
    assert.equal(outputs.has_changesets, "true");
    assert.deepEqual(JSON.parse(outputs.changesets), ["change"]);
    assert.equal(outputs.has_servers, "true");
    assert.equal(outputs.has_static_sites, "false");
    assert.deepEqual(JSON.parse(outputs.releases), [
      { name: "accounts-api", version: "0.1.0" },
    ]);
    assert.equal(JSON.parse(outputs.servers)[0].service, "accounts-api");
    assert.match(readFileSync(summaryPath, "utf8"), /accounts-api@0\.1\.0/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("check and the --check CLI reject non-target deploy.target values", () => {
  const cli = fileURLToPath(new URL("./deploy-plan.mjs", import.meta.url));
  const cases = [
    ["servers/api", "api", { target: ["droplet"], project: "p" }],
    ["servers/api", "api", { target: "__proto__", service: "s" }],
    ["apps/web", "web", { target: "constructor", project: "web" }],
    ["apps/web", "web", { target: ["cloudflare-pages"], project: "web" }],
  ];
  for (const [dir, name, deploy] of cases) {
    const workDir = mkdtempSync(join(tmpdir(), "deploy-plan-target-"));
    try {
      mkdirSync(join(workDir, ".changeset"));
      writeFileSync(join(workDir, ".changeset", "brave-cats.md"), "---\n---\n");
      mkdirSync(join(workDir, dir), { recursive: true });
      writeFileSync(
        join(workDir, dir, "package.json"),
        JSON.stringify({ name, deploy }),
      );
      const label = JSON.stringify(deploy);
      assert.throws(
        () => check(workDir),
        (error) => {
          assert.ok(error instanceof DeployPlanError, label);
          assert.match(error.message, invalidTargetMessage, label);
          return true;
        },
        label,
      );
      const run = spawnSync(process.execPath, [cli, "--check", workDir], {
        encoding: "utf8",
      });
      assert.equal(run.status, 2, label);
      const event = JSON.parse(run.stderr.trim());
      assert.equal(event.event, "deploy_plan_error", label);
      assert.match(event.message, invalidTargetMessage, label);
    } finally {
      rmSync(workDir, { recursive: true, force: true });
    }
  }
});

test("check accepts the repository and rejects bad changeset file names", () => {
  assert.doesNotThrow(() => check(root));
  const dir = mkdtempSync(join(tmpdir(), "deploy-plan-check-"));
  try {
    mkdirSync(join(dir, ".changeset"));
    writeFileSync(join(dir, ".changeset", "README.md"), "# x\n");
    writeFileSync(join(dir, ".changeset", "brave-cats.md"), "---\n---\n");
    assert.deepEqual(check(dir), ["brave-cats"]);
    writeFileSync(join(dir, ".changeset", "release notes.md"), "---\n---\n");
    assert.throws(() => check(dir), /"release notes" is not a plain file name/);
    rmSync(join(dir, ".changeset", "release notes.md"));
    // The Changesets reader also reads `.changeset/pre/*.md` as `pre/<id>`.
    mkdirSync(join(dir, ".changeset", "pre"));
    writeFileSync(join(dir, ".changeset", "pre", "fix-api.md"), "---\n---\n");
    assert.throws(() => check(dir), /"pre\/fix-api" is not a plain file name/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("unnamed non-deployable packages do not break dependency ordering", () => {
  const packages = [
    pkg("tools/unnamed", {
      private: true,
      dependencies: { self: "workspace:../unnamed" },
    }),
    pkg("servers/api", {
      name: "api",
      dependencies: {
        lodash: "^4.0.0",
        helper: "workspace:../../tools/unnamed",
      },
      deploy: droplet("api-svc"),
    }),
    pkg("servers/billing", {
      name: "billing",
      dependencies: { api: "workspace:*" },
      deploy: droplet("billing"),
    }),
  ];
  const result = plan(status(["api", "1.0.1"], ["billing", "1.0.1"]), packages);
  assert.deepEqual(
    result.servers.map((d) => d.name),
    ["api", "billing"],
  );
});

test("an unnamed package with a relative workspace link is ignored", () => {
  const packages = [
    pkg("tools/unnamed", {
      private: true,
      dependencies: { self: "workspace:../unnamed" },
    }),
    pkg("servers/api", {
      name: "api",
      dependencies: { helper: "workspace:../../tools/unnamed" },
      deploy: droplet("api-svc"),
    }),
  ];
  const result = plan(status(["api", "1.0.1"]), packages);
  assert.deepEqual(
    result.servers.map((d) => d.name),
    ["api"],
  );
});

test("real workspace cycles still fail next to unnamed packages", () => {
  const packages = [
    pkg("tools/unnamed", { private: true }),
    pkg("servers/a", {
      name: "a",
      dependencies: { b: "workspace:*" },
      deploy: droplet("a-svc"),
    }),
    pkg("servers/b", {
      name: "b",
      dependencies: { a: "workspace:*" },
      deploy: droplet("b-svc"),
    }),
  ];
  assert.throws(
    () => plan(status(["a", "1.0.1"], ["b", "1.0.1"]), packages),
    /dependency cycle: a -> b -> a/,
  );
});

test("an unnamed deployable still fails beside named packages", () => {
  const packages = [
    pkg("servers/api", { name: "api", deploy: droplet("api-svc") }),
    pkg("servers/nameless", { deploy: droplet("other") }),
  ];
  assert.throws(
    () => plan(status(["api", "1.0.1"]), packages),
    /servers\/nameless: a deployable package name must be a string/,
  );
});
