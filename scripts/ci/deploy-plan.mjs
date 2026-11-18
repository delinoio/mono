// SPDX-License-Identifier: Apache-2.0
//
// Selects the deployables of one `deploy.yml` run from the pending changesets
// and orders them by dependency. Input is the JSON file written by
// `changeset status --output <file>`; the release list in it already includes
// the dependents of each released package. Only workspace packages with a
// `deploy` manifest field are deployed. Every release whose version advances
// is tagged after the deployment, deployable or not. A `none` release (a
// dependent through `devDependencies`, or an explicit `none` changeset) is
// deployed when it is deployable, but keeps its version and gets no tag.
import { appendFileSync, readdirSync, readFileSync, existsSync } from "node:fs";
import { join } from "node:path";
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

// Kind order of a deployment: the database is migrated outside this workflow,
// servers come next and static sites, which call the servers, come last.
const kindOrder = [DeployKind.Server, DeployKind.StaticSite];

const targetRules = {
  [DeployTarget.Railway]: { kind: DeployKind.Server, root: "servers" },
  [DeployTarget.CloudflarePages]: { kind: DeployKind.StaticSite, root: "apps" },
};

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
  const rule = targetRules[deploy?.target];
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
  const output = deploy.output ?? "dist";
  if (
    typeof output !== "string" ||
    output.startsWith("/") ||
    output.split("/").includes("..")
  ) {
    throw new DeployPlanError(
      `${name}: deploy.output must be a package-relative path`,
    );
  }
  return { ...base, project: deploy.project, output: `${pkg.dir}/${output}` };
}

// Orders the selected names so that every workspace dependency comes before
// its dependents. Ties keep alphabetical order, so the result is stable.
function topologicalOrder(selected, packagesByName) {
  const ordered = [];
  const state = new Map();
  const visit = (name, path) => {
    if (state.get(name) === "done") return;
    if (state.get(name) === "visiting") {
      throw new DeployPlanError(
        `workspace dependency cycle: ${[...path, name].join(" -> ")}`,
      );
    }
    state.set(name, "visiting");
    const manifest = packagesByName.get(name).manifest;
    const deps = new Set(
      dependencyFields.flatMap((field) => Object.keys(manifest[field] ?? {})),
    );
    for (const dep of [...deps].sort()) {
      if (packagesByName.has(dep)) visit(dep, [...path, name]);
    }
    state.set(name, "done");
    if (selected.has(name)) ordered.push(name);
  };
  for (const name of [...selected].sort()) visit(name, []);
  return ordered;
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
  const ordered = topologicalOrder(new Set(versions.keys()), packagesByName);
  const deployables = ordered
    .map((name) => deployable(packagesByName.get(name)))
    .filter(Boolean)
    .map((d) => ({ ...d, version: versions.get(d.name) }));
  deployables.sort(
    (a, b) => kindOrder.indexOf(a.kind) - kindOrder.indexOf(b.kind),
  );
  return {
    // The deploy jobs check that these changeset files are still pending on
    // `main` before they upload, so a rerun of an old run cannot deploy a
    // superseded revision.
    changesets: (status.changesets ?? []).map(({ id }) => {
      if (typeof id !== "string" || !/^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(id)) {
        throw new DeployPlanError(
          `changeset id ${JSON.stringify(id)} is not a plain file name`,
        );
      }
      return id;
    }),
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
  if (!statusPath) {
    throw new DeployPlanError("usage: deploy-plan.mjs <changeset-status.json>");
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
