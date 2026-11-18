// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import {
  DeployKind,
  DeployPlanError,
  DeployTarget,
  ReleaseType,
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

test("an unknown deploy target fails", () => {
  const bad = [pkg("servers/api", { name: "api", deploy: { target: "fly" } })];
  assert.throws(() => plan(status(["api", "1.0.0"]), bad), /deploy.target/);
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
