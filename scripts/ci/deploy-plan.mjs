// SPDX-License-Identifier: Apache-2.0
//
// Selects the deployables of one `deploy.yml` run from the pending changesets
// and orders them by dependency. Input is the JSON file written by
// `changeset status --output <file>`. Changesets adds some dependents of a
// released package to that list, but not all (see the deployment workflow
// contract), so maintainers select dependents explicitly. Only workspace
// packages with a
// `deploy` manifest field are deployed. Every release whose version advances
// is tagged after the deployment, deployable or not. A `none` release (a
// dependent through `devDependencies`, or an explicit `none` changeset) is
// deployed when it is deployable, but keeps its version and gets no tag.
import { appendFileSync, readdirSync, readFileSync, existsSync } from "node:fs";
import { join, posix } from "node:path";
import { fileURLToPath } from "node:url";
import { workspaceRoots } from "./plan.mjs";

// Stable deployment target identifiers of the `deploy.target` manifest field.
export const DeployTarget = Object.freeze({
  Railway: "railway",
  CloudflarePages: "cloudflare-pages",
});

// Deployable kinds. Each kind maps to exactly one GitHub deployment
// environment (see docs/repository-workflow-contract.md).
export const DeployKind = Object.freeze({
  Server: "server",
  StaticSite: "static-site",
});

// Kind order of a deployment: the database stage (not implemented yet; see
// the deployment workflow contract) comes first, then servers, then static
// sites, which call the servers.
const kindOrder = [DeployKind.Server, DeployKind.StaticSite];

// A Map, not a plain object: `Map.get` matches by SameValueZero, so inherited
// `Object.prototype` members (`constructor`, `__proto__`, ...) and arrays that
// coerce to a valid key (`["railway"]`) never resolve to a rule.
const targetRules = new Map([
  [DeployTarget.Railway, { kind: DeployKind.Server, root: "servers" }],
  [DeployTarget.CloudflarePages, { kind: DeployKind.StaticSite, root: "apps" }],
]);

const dependencyFields = [
  "dependencies",
  "devDependencies",
  "peerDependencies",
  "optionalDependencies",
];

// Changesets release types that the planner treats differently.
export const ReleaseType = Object.freeze({
  None: "none",
});

const deployableName = /^[a-z0-9][a-z0-9._-]*$/;

export class DeployPlanError extends Error {}

// Reads every workspace package directly inside a workspace root.
export function readWorkspacePackages(repoRoot) {
  const packages = [];
  for (const workspaceRoot of workspaceRoots) {
    const rootPath = join(repoRoot, workspaceRoot);
    if (!existsSync(rootPath)) continue;
    for (const entry of readdirSync(rootPath, { withFileTypes: true })) {
      if (!entry.isDirectory()) continue;
      const manifestPath = join(rootPath, entry.name, "package.json");
      if (!existsSync(manifestPath)) continue;
      packages.push({
        dir: `${workspaceRoot}/${entry.name}`,
        manifest: JSON.parse(readFileSync(manifestPath, "utf8")),
      });
    }
  }
  return packages;
}

function deployable(pkg) {
  const { name, deploy } = pkg.manifest;
  if (deploy === undefined) return undefined;
  const rule = targetRules.get(deploy?.target);
  if (!rule) {
    throw new DeployPlanError(
      `${name}: deploy.target must be one of ${Object.values(DeployTarget).join(", ")}`,
    );
  }
  // The workflow uses the package name in file paths, artifact names and
  // tags, so a deployable name must not contain a scope or a slash.
  if (!deployableName.test(name)) {
    throw new DeployPlanError(
      `${name}: a deployable package name must match ${deployableName}`,
    );
  }
  if (!pkg.dir.startsWith(`${rule.root}/`)) {
    throw new DeployPlanError(
      `${name}: deploy.target ${deploy.target} is allowed only under ${rule.root}/`,
    );
  }
  const base = { name, dir: pkg.dir, kind: rule.kind, target: deploy.target };
  if (deploy.target === DeployTarget.Railway) {
    if (typeof deploy.service !== "string" || deploy.service === "") {
      throw new DeployPlanError(`${name}: deploy.service is required`);
    }
    return { ...base, service: deploy.service };
  }
  if (typeof deploy.project !== "string" || deploy.project === "") {
    throw new DeployPlanError(`${name}: deploy.project is required`);
  }
  const raw = deploy.output ?? "dist";
  // The output must be a directory inside the package, never the package
  // root itself (`""` or `"."`) or a path outside it.
  const output =
    typeof raw === "string" ? posix.normalize(raw).replace(/\/+$/, "") : "";
  if (
    output === "" ||
    output === "." ||
    output.startsWith("/") ||
    output === ".." ||
    output.startsWith("../")
  ) {
    throw new DeployPlanError(
      `${name}: deploy.output must be a directory inside the package`,
    );
  }
  return { ...base, project: deploy.project, output: `${pkg.dir}/${output}` };
}

// Returns the workspace package that a dependency entry links, or undefined
// when it links none. pnpm links a workspace package only through the
// `workspace:` protocol (`linkWorkspacePackages` is off), so a plain range or
// a registry alias such as `npm:lodash@4` never links one, even when its key
// names a workspace package. A pnpm alias links another package under the
// key: `"provider": "workspace:z@*"` names package `z`, and
// `"client": "workspace:../z"` names the package in that directory, relative
// to the declaring package.
function workspaceTarget(
  key,
  spec,
  declaringDir,
  packagesByName,
  packagesByDir,
) {
  if (typeof spec !== "string" || !spec.startsWith("workspace:"))
    return undefined;
  const target = spec.slice("workspace:".length);
  if (target.startsWith("./") || target.startsWith("../")) {
    const pkg = packagesByDir.get(
      posix.normalize(posix.join(declaringDir, target)).replace(/\/+$/, ""),
    );
    return pkg?.manifest.name;
  }
  const alias = /^(@?[^@]+)@/.exec(target);
  if (alias && packagesByName.has(alias[1])) return alias[1];
  return key;
}

// Orders the selected names so that every workspace dependency, direct or
// through unselected packages, comes before its dependents. Among the
// packages whose dependencies are already ordered, the alphabetically first
// one comes next, so the result is stable.
function topologicalOrder(selected, packagesByName) {
  const packagesByDir = new Map(
    [...packagesByName.values()].map((pkg) => [pkg.dir, pkg]),
  );
  // Selected packages that each package depends on, transitively.
  const reach = new Map();
  const state = new Map();
  const visit = (name, path) => {
    if (state.get(name) === "done") return reach.get(name);
    if (state.get(name) === "visiting") {
      throw new DeployPlanError(
        `workspace dependency cycle: ${[...path, name].join(" -> ")}`,
      );
    }
    state.set(name, "visiting");
    const { manifest, dir } = packagesByName.get(name);
    const deps = new Set(
      dependencyFields.flatMap((field) =>
        Object.entries(manifest[field] ?? {}).map(([key, spec]) =>
          workspaceTarget(key, spec, dir, packagesByName, packagesByDir),
        ),
      ),
    );
    const reached = new Set();
    for (const dep of [...deps].sort()) {
      if (!packagesByName.has(dep)) continue;
      if (selected.has(dep)) reached.add(dep);
      for (const transitive of visit(dep, [...path, name])) {
        reached.add(transitive);
      }
    }
    state.set(name, "done");
    reach.set(name, reached);
    return reached;
  };
  for (const name of [...selected].sort()) visit(name, []);

  const ordered = [];
  const remaining = new Set(selected);
  while (remaining.size > 0) {
    const next = [...remaining]
      .filter((name) =>
        [...reach.get(name)].every((dep) => !remaining.has(dep)),
      )
      .sort()[0];
    ordered.push(next);
    remaining.delete(next);
  }
  return ordered;
}

// The deploy jobs use changeset ids in API paths and output maps.
function checkChangesetId(id) {
  if (typeof id !== "string" || !/^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(id)) {
    throw new DeployPlanError(
      `changeset id ${JSON.stringify(id)} is not a plain file name`,
    );
  }
  return id;
}

// Matches the files that @changesets/read (1.0.1, used by @changesets/cli
// 3.0.3) reads as changesets, by base name.
function isChangesetFile(file) {
  return (
    !file.startsWith(".") &&
    file.endsWith(".md") &&
    !/^README\.md$/i.test(file) &&
    !["AGENTS.md", "CLAUDE.md", "GEMINI.md"].includes(file)
  );
}

// Applies the plan rules that do not depend on a release to the repository:
// every pending changeset file name and every deployable manifest. CI runs
// this, so a change that would stop every deployment fails in its own pull
// request.
export function check(repoRoot) {
  // The reader also reads `.changeset/pre/*.md`, with the id `pre/<name>`.
  const list = (dir, prefix) =>
    existsSync(dir)
      ? readdirSync(dir)
          .filter(isChangesetFile)
          .map((file) => prefix + file.slice(0, -".md".length))
      : [];
  const changesetDir = join(repoRoot, ".changeset");
  const ids = [
    ...list(changesetDir, ""),
    ...list(join(changesetDir, "pre"), "pre/"),
  ];
  for (const id of ids) checkChangesetId(id);
  plan({ changesets: [], releases: [] }, readWorkspacePackages(repoRoot));
  return ids;
}

// Two deployables that name the same Railway service or Pages project would
// overwrite each other, while both get a release tag. Checks every
// deployable of the workspace, selected or not.
function rejectSharedDestinations(packages) {
  const owners = new Map();
  for (const pkg of packages) {
    const d = deployable(pkg);
    if (!d) continue;
    const key = `${d.target}:${d.service ?? d.project}`;
    const owner = owners.get(key);
    if (owner) {
      throw new DeployPlanError(`${owner} and ${d.name} both deploy to ${key}`);
    }
    owners.set(key, d.name);
  }
}

// Builds the deployment plan from the `changeset status` JSON and the
// workspace packages.
export function plan(status, packages) {
  const packagesByName = new Map(
    packages.map((pkg) => [pkg.manifest.name, pkg]),
  );
  const allReleases = (status.releases ?? []).map(
    ({ name, type, newVersion }) => {
      if (!packagesByName.has(name)) {
        throw new DeployPlanError(`release ${name} is not a workspace package`);
      }
      return { name, type, version: newVersion };
    },
  );
  // A `none` release keeps its current version, whose tag may already exist.
  const releases = allReleases
    .filter((r) => r.type !== ReleaseType.None)
    .map(({ name, version }) => ({ name, version }));
  const versions = new Map(allReleases.map((r) => [r.name, r.version]));
  rejectSharedDestinations(packages);
  const byName = new Map(
    [...versions.keys()]
      .map((name) => deployable(packagesByName.get(name)))
      .filter(Boolean)
      .map((d) => [d.name, { ...d, version: versions.get(d.name) }]),
  );
  // Order each deployment stage on its own: dependencies in an earlier stage
  // are already deployed, so the alphabetical tie-break applies within the
  // stage.
  const deployables = kindOrder.flatMap((kind) => {
    const names = [...byName.values()]
      .filter((d) => d.kind === kind)
      .map((d) => d.name);
    return topologicalOrder(new Set(names), packagesByName).map((name) =>
      byName.get(name),
    );
  });
  return {
    // The deploy jobs check that these changeset files are still pending on
    // `main` before they upload, so a rerun of an old run cannot deploy a
    // superseded revision.
    changesets: (status.changesets ?? []).map(({ id }) => checkChangesetId(id)),
    releases: releases.sort((a, b) => a.name.localeCompare(b.name)),
    servers: deployables.filter((d) => d.kind === DeployKind.Server),
    staticSites: deployables.filter((d) => d.kind === DeployKind.StaticSite),
  };
}

export function main({
  argv = process.argv.slice(2),
  env = process.env,
  repoRoot = fileURLToPath(new URL("../..", import.meta.url)),
} = {}) {
  const [statusPath] = argv;
  if (statusPath === "--check") {
    const ids = check(argv[1] ?? repoRoot);
    console.log(
      JSON.stringify({ event: "deploy_plan_check", changesets: ids }),
    );
    return undefined;
  }
  if (!statusPath) {
    throw new DeployPlanError(
      "usage: deploy-plan.mjs <changeset-status.json> | --check [repo-root]",
    );
  }
  const status = JSON.parse(readFileSync(statusPath, "utf8"));
  const result = plan(status, readWorkspacePackages(repoRoot));
  console.log(
    JSON.stringify({
      event: "deploy_plan",
      changesets: result.changesets,
      releases: result.releases,
      servers: result.servers.map((d) => d.name),
      staticSites: result.staticSites.map((d) => d.name),
    }),
  );
  if (env.GITHUB_OUTPUT) {
    appendFileSync(
      env.GITHUB_OUTPUT,
      [
        `releases=${JSON.stringify(result.releases)}`,
        `servers=${JSON.stringify(result.servers)}`,
        `static_sites=${JSON.stringify(result.staticSites)}`,
        // Any pending changeset, including one with only `none` releases,
        // needs the release job to consume it.
        `changesets=${JSON.stringify(result.changesets)}`,
        `has_changesets=${result.changesets.length > 0}`,
        `has_servers=${result.servers.length > 0}`,
        `has_static_sites=${result.staticSites.length > 0}`,
        "",
      ].join("\n"),
    );
  }
  if (env.GITHUB_STEP_SUMMARY) {
    const lines = ["## Deployment plan", ""];
    if (result.changesets.length === 0) {
      lines.push("No pending changesets. Nothing to deploy.");
    } else {
      lines.push("| Order | Package | Version | Kind | Target |");
      lines.push("| --- | --- | --- | --- | --- |");
      [...result.servers, ...result.staticSites].forEach((d, i) => {
        lines.push(
          `| ${i + 1} | ${d.name} | ${d.version} | ${d.kind} | ${d.target} |`,
        );
      });
      lines.push("");
      lines.push(
        `Versioned and tagged: ${result.releases.map((r) => `${r.name}@${r.version}`).join(", ") || "none"}`,
      );
    }
    appendFileSync(env.GITHUB_STEP_SUMMARY, `${lines.join("\n")}\n`);
  }
  return result;
}

if (import.meta.main) {
  try {
    main();
  } catch (error) {
    console.error(
      JSON.stringify({ event: "deploy_plan_error", message: error.message }),
    );
    process.exitCode = 2;
  }
}
