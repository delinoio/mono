// SPDX-License-Identifier: Apache-2.0
//
// Workspace package discovery and release input paths that the change planner
// (`plan.mjs`) and the deploy planner (`deploy-plan.mjs`) share. This module
// imports neither of them, so both can import it without a module cycle.
import {
  existsSync,
  lstatSync,
  readdirSync,
  readFileSync,
  readlinkSync,
} from "node:fs";
import { isAbsolute, join, posix, relative, sep } from "node:path";

// Workspace package roots, as directories that hold one level of packages.
// `protos/delino` holds the TypeScript binding packages, one per schema
// directory. Keep these roots in sync with `pnpm-workspace.yaml`;
// plan.test.mjs cross-checks them.
export const workspaceRoots = ["apps", "packages", "protos/delino", "servers"];

// Single workspace packages outside the workspace roots: the release-only Go
// command packages under `cmds/`. Their manifests declare no package tasks
// and exist so that Changesets versions them and `deploy.yml` releases them
// (see the deployment workflow contract), so only the manifest, not the Go
// source, is a workspace input. plan.test.mjs checks that they declare no
// scripts and that `pnpm-workspace.yaml` lists exactly these directories.
export const workspacePackageDirs = [
  "cmds/ownsand",
  "cmds/ownsand-agent",
  "cmds/sandrun",
];

// Reads the manifest of every workspace package under `repoRoot`.
export function readWorkspacePackages(
  repoRoot,
  { skipUnreadableManifests = false } = {},
) {
  const packages = [];
  const addPackage = (dir) => {
    const manifestPath = join(repoRoot, dir, "package.json");
    if (!existsSync(manifestPath)) return;
    try {
      const manifest = JSON.parse(readFileSync(manifestPath, "utf8"));
      packages.push({ dir, manifest });
    } catch (error) {
      if (!skipUnreadableManifests) throw error;
    }
  };
  for (const dir of workspacePackageDirs) {
    addPackage(dir);
  }
  for (const workspaceRoot of workspaceRoots) {
    const rootPath = join(repoRoot, workspaceRoot);
    if (!existsSync(rootPath)) continue;
    for (const entry of readdirSync(rootPath, { withFileTypes: true })) {
      if (entry.isDirectory()) addPackage(`${workspaceRoot}/${entry.name}`);
    }
  }
  return packages;
}

// Control characters and backslashes break the tab- and line-separated
// transports of the deploy scripts.
const transportUnsafe = /[\u0000-\u001f\u007f\\]/;

// Normalizes a relative path and returns it, or undefined when it is empty,
// absolute, outside its base or unsafe for the deploy scripts.
export function relativePath(raw) {
  if (typeof raw !== "string" || transportUnsafe.test(raw)) return undefined;
  const path = posix.normalize(raw).replace(/\/+$/, "");
  if (
    path === "" ||
    path === "." ||
    path.startsWith("/") ||
    path === ".." ||
    path.startsWith("../")
  ) {
    return undefined;
  }
  return path;
}

// The repository-relative path of a `deploy.entitlements` value, which is
// relative to its package directory, or undefined when it is not a safe path.
export function entitlementsPath(dir, raw) {
  const path = relativePath(raw);
  return path === undefined ? undefined : `${dir}/${path}`;
}

// Walk each path component so we can record links to directories and retain
// missing destinations. Deployment validation checks the final file.
function* entitlementsInputs(repoRoot, declaredPath) {
  yield declaredPath;
  let parent = ".";
  let remaining = declaredPath.split("/");
  const seen = new Set();
  while (remaining.length > 0) {
    const [part, ...rest] = remaining;
    remaining = rest;
    if (part === "" || part === ".") continue;
    if (part === "..") {
      if (parent === ".") return;
      parent = posix.dirname(parent);
      continue;
    }

    const path = posix.join(parent, part);
    const absolute = join(repoRoot, path);
    try {
      const stat = lstatSync(absolute, { throwIfNoEntry: false });
      if (!stat?.isDirectory() || remaining.length === 0) yield path;
      if (!stat) {
        // Removing a tracked target can also remove its empty parent.
        const destination = posix.join(path, ...remaining);
        if (destination !== ".." && !destination.startsWith("../")) {
          yield destination;
        }
        return;
      }
      if (stat.isDirectory()) {
        parent = path;
        continue;
      }
      if (!stat.isSymbolicLink()) return;

      const state = [path, ...remaining].join("/");
      // The hop limit also bounds cycles that grow at each expansion.
      if (seen.has(state) || seen.size === 64) return;
      seen.add(state);

      let target = readlinkSync(absolute);
      if (isAbsolute(target)) {
        target = relative(repoRoot, target);
        if (isAbsolute(target)) return;
        parent = ".";
      }
      if (sep === "\\") target = target.replaceAll("\\", "/");
      remaining = [...target.split("/"), ...remaining];
    } catch {
      return;
    }
  }
}

// The repository-relative paths that a `github-release` package declares as
// release files: its entitlements path and its license file paths. The release
// workflow ships these files, so a change to one of them must run the deploy
// checks. An invalid declaration contributes no path here; the deploy planner
// rejects it, and a change to its manifest selects those checks already.
function declaredReleasePaths(repoRoot, { dir, manifest }) {
  const deploy = manifest?.deploy;
  if (deploy?.target !== "github-release") return [];
  const paths = [];
  const entitlements = entitlementsPath(dir, deploy.entitlements);
  if (entitlements !== undefined) {
    paths.push(...entitlementsInputs(repoRoot, entitlements));
  }
  if (Array.isArray(deploy.licenseFiles)) {
    for (const file of deploy.licenseFiles) {
      const path = relativePath(file);
      if (path !== undefined) paths.push(path);
    }
  }
  return paths;
}

// The set of release file paths that the `github-release` packages of the
// checkout at `repoRoot` declare. A manifest that cannot be read selects the
// fixtures through its own path, so an unreadable one declares nothing here.
export function declaredReleaseInputs(repoRoot) {
  let packages;
  try {
    packages = readWorkspacePackages(repoRoot, {
      skipUnreadableManifests: true,
    });
  } catch {
    return new Set();
  }
  return new Set(
    packages.flatMap((pkg) => declaredReleasePaths(repoRoot, pkg)),
  );
}
