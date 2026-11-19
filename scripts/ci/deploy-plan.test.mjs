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
  check,
  main,
  plan,
  readWorkspacePackages,
} from "./deploy-plan.mjs";

const root = fileURLToPath(new URL("../..", import.meta.url));

const pkg = (dir, manifest) => ({ dir, manifest });

const workspace = [
  pkg("packages/ui", { name: "ui" }),
  pkg("servers/api", {
    name: "api",
    deploy: { target: DeployTarget.Railway, service: "api-svc" },
  }),
  pkg("servers/billing", {
    name: "billing",
    dependencies: { api: "workspace:*" },
    deploy: { target: DeployTarget.Railway, service: "billing" },
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
    target: DeployTarget.Railway,
    service: "api-svc",
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
  for (const [target, field] of [
    [DeployTarget.CloudflarePages, "project"],
    [DeployTarget.Railway, "service"],
  ]) {
    const root = target === DeployTarget.Railway ? "servers" : "apps";
    const shared = [
      pkg(`${root}/one`, {
        name: "one",
        deploy: { target, [field]: "shared" },
      }),
      pkg(`${root}/two`, {
        name: "two",
        deploy: { target, [field]: "shared" },
      }),
    ];
    assert.throws(
      () => plan(status(["one", "1.0.0"]), shared),
      /one and two both deploy to .*:shared/,
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
      deploy: { target: DeployTarget.Railway, service: "api" },
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
      deploy: { target: DeployTarget.Railway, service: "api" },
    }),
  ];
  assert.throws(
    () => plan(status(["@delino/api", "1.0.0"]), scoped),
    /deployable package name/,
  );
});

const invalidTargetMessage =
  /^(web|api): deploy\.target must be one of railway, cloudflare-pages$/;

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
  ["railway"],
  ["cloudflare-pages"],
  "",
  null,
  7,
  { toString: () => "railway" },
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
    [["api", DeployTarget.Railway]],
  );
  assert.deepEqual(
    result.staticSites.map((d) => [d.name, d.target]),
    [["web", DeployTarget.CloudflarePages]],
  );
  const wrongRoot = [
    pkg("apps/web", {
      name: "web",
      deploy: { target: DeployTarget.Railway, service: "web" },
    }),
  ];
  assert.throws(
    () => plan(status(["web", "1.0.0"]), wrongRoot),
    /web: deploy\.target railway is allowed only under servers\//,
  );
});

test("missing service or project names fail", () => {
  const server = [
    pkg("servers/api", {
      name: "api",
      deploy: { target: DeployTarget.Railway },
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
      deploy: { target: DeployTarget.Railway, service: "z-api" },
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
      deploy: { target: DeployTarget.Railway, service: "a" },
    }),
    pkg("servers/z", {
      name: "z",
      deploy: { target: DeployTarget.Railway, service: "z" },
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
      deploy: { target: DeployTarget.Railway, service: "a" },
    }),
    pkg("servers/b", {
      name: "b",
      dependencies: { a: "workspace:*" },
      deploy: { target: DeployTarget.Railway, service: "b" },
    }),
    pkg("servers/c", {
      name: "c",
      dependencies: { a: "workspace:^" },
      deploy: { target: DeployTarget.Railway, service: "c" },
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
      deploy: { target: DeployTarget.Railway, service: "a" },
    }),
    pkg("servers/b", {
      name: "b",
      dependencies: { a: "workspace:*" },
      deploy: { target: DeployTarget.Railway, service: "b" },
    }),
  ];
  assert.throws(
    () => plan(status(["a", "1.0.0"]), cyclic),
    /workspace dependency cycle/,
  );
});

test("repository deployables use the documented targets", () => {
  const packages = readWorkspacePackages(root);
  const all = packages.map((p) => [p.manifest.name, "1.0.0"]);
  const result = plan(status(...all), packages);
  assert.deepEqual(
    result.servers.map((d) => [d.name, d.service]),
    [["accounts-api", "accounts-api"]],
  );
  assert.deepEqual(
    result.staticSites.map((d) => [d.name, d.project, d.output]),
    [
      ["delino-accounts", "delino-accounts", "apps/delino-accounts/dist"],
      ["delino-docs", "delino-docs", "apps/delino-docs/dist"],
      ["delino-site", "delino-site", "apps/delino-site/dist"],
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
    ["servers/api", "api", { target: ["railway"], project: "p" }],
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
