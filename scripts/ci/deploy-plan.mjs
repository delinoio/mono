// SPDX-License-Identifier: Apache-2.0
//
// Selects the deployables of one `deploy.yml` run from the pending changesets
// and orders them by dependency. Input is the JSON file written by
// `changeset status --output <file>`. Changesets adds some dependents of a
// released package to that list, but not all (see the deployment workflow
// contract), so maintainers select dependents explicitly. Only workspace
// packages with a
// `deploy` manifest field are deployed. Every release whose version advances
// is tagged after the deployment, deployable or not; binary releases
// (`github-release`) are published from their tags after that. A `none` release (a
// dependent through `devDependencies`, or an explicit `none` changeset) is
// deployed when it is deployable, but keeps its version and gets no tag.
import {
  appendFileSync,
  existsSync,
  lstatSync,
  readdirSync,
  readFileSync,
  statSync,
} from "node:fs";
import { join, posix } from "node:path";
import { fileURLToPath } from "node:url";
import {
  entitlementsPath,
  readWorkspacePackages,
  relativePath,
} from "./workspace-packages.mjs";

export { readWorkspacePackages };

// Stable deployment target identifiers of the `deploy.target` manifest field.
export const DeployTarget = Object.freeze({
  Droplet: "droplet",
  CloudflarePages: "cloudflare-pages",
  GithubRelease: "github-release",
});

// Deployable kinds. Each kind maps to exactly one GitHub deployment
// environment (see docs/repository-workflow-contract.md).
export const DeployKind = Object.freeze({
  Server: "server",
  StaticSite: "static-site",
  Binary: "binary",
});

// Kind order of a deployment: servers, then static sites, which call the
// servers, then binaries, which are published from their release tags after
// the release job. There is no database stage: each server applies its own
// embedded migrations at startup (see the deployment workflow contract).
const kindOrder = [DeployKind.Server, DeployKind.StaticSite, DeployKind.Binary];

// A Map, not a plain object: `Map.get` matches by SameValueZero, so inherited
// `Object.prototype` members (`constructor`, `__proto__`, ...) and arrays that
// coerce to a valid key (`["droplet"]`) never resolve to a rule.
const targetRules = new Map([
  [DeployTarget.Droplet, { kind: DeployKind.Server, root: "servers" }],
  [DeployTarget.CloudflarePages, { kind: DeployKind.StaticSite, root: "apps" }],
  [DeployTarget.GithubRelease, { kind: DeployKind.Binary, root: "cmds" }],
]);

// Release platforms (`<GOOS>-<GOARCH>`) of a `github-release` deployable. The
// release manifest tool (cmds/ownsand-agent/release-manifest) accepts the same
// set.
export const ReleasePlatform = Object.freeze({
  LinuxAmd64: "linux-amd64",
  LinuxArm64: "linux-arm64",
  DarwinAmd64: "darwin-amd64",
  DarwinArm64: "darwin-arm64",
  WindowsAmd64: "windows-amd64",
  WindowsArm64: "windows-arm64",
});

// Platforms that the deploy workflow can build with cgo: only on a native
// runner, and the macOS runner is arm64.
const cgoPlatforms = new Set([ReleasePlatform.DarwinArm64]);

// The executable name, which also names the release assets.
const binaryName = /^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/;

// The release assets that the workflow adds to every binary release. A
// license file must not have one of these names.
const reservedAssetNames = new Set([
  "SHA256SUMS",
  "GO-LICENSE",
  "THIRD-PARTY-LICENSES.txt",
]);

// A Go symbol for `go build -ldflags -X`: `<import path>.<name>`.
const goSymbol = /^[A-Za-z0-9_.\/-]+\.[A-Za-z_][A-Za-z0-9_]*$/;

// The release manifest object key inside the releases bucket.
const manifestObjectKey = /^(?!.*\.\.)(?!.*\/\/)[a-z0-9][a-z0-9._\/-]*\.json$/;

// The agent accepts only MAJOR.MINOR.PATCH as an update target, so a binary
// release must not have a prefix or a pre-release suffix.
const binaryReleaseVersion =
  /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/;

// A nonempty list of different strings, or undefined.
function stringList(value) {
  if (
    !Array.isArray(value) ||
    value.length === 0 ||
    !value.every((v) => typeof v === "string" && v !== "") ||
    new Set(value).size !== value.length
  ) {
    return undefined;
  }
  return value;
}

function assetNames(binary, platforms) {
  return platforms.map((p) =>
    p.startsWith("windows-") ? `${binary}-${p}.exe` : `${binary}-${p}`,
  );
}

// Validates the fields of a `github-release` deployable.
function binaryFields(name, deploy, dir) {
  if (typeof deploy.binary !== "string" || !binaryName.test(deploy.binary)) {
    throw new DeployPlanError(
      `${name}: deploy.binary must be an executable name matching ${binaryName}`,
    );
  }
  const known = Object.values(ReleasePlatform);
  const platforms = stringList(deploy.platforms);
  if (!platforms || !platforms.every((p) => known.includes(p))) {
    throw new DeployPlanError(
      `${name}: deploy.platforms must be a nonempty list of different platforms from ${known.join(", ")}`,
    );
  }
  const cgo =
    deploy.cgoPlatforms === undefined ? [] : stringList(deploy.cgoPlatforms);
  if (!cgo || !cgo.every((p) => platforms.includes(p) && cgoPlatforms.has(p))) {
    throw new DeployPlanError(
      `${name}: deploy.cgoPlatforms must be a nonempty list of different entries of deploy.platforms from ${[...cgoPlatforms].join(", ")}`,
    );
  }
  let versionVariable = "";
  if (deploy.versionVariable !== undefined) {
    versionVariable = deploy.versionVariable;
    if (
      typeof versionVariable !== "string" ||
      !goSymbol.test(versionVariable) ||
      !versionVariable.startsWith(`github.com/delinoio/mono/${dir}/`)
    ) {
      throw new DeployPlanError(
        `${name}: deploy.versionVariable must be a Go variable <import path>.<name> inside github.com/delinoio/mono/${dir}/`,
      );
    }
  }
  let entitlements = "";
  if (deploy.entitlements !== undefined) {
    const path = entitlementsPath(dir, deploy.entitlements);
    if (!path || !path.endsWith(".plist")) {
      throw new DeployPlanError(
        `${name}: deploy.entitlements must be a .plist file inside the package`,
      );
    }
    if (!platforms.some((p) => p.startsWith("darwin-"))) {
      throw new DeployPlanError(
        `${name}: deploy.entitlements needs a darwin platform`,
      );
    }
    entitlements = path;
  }
  const rawLicenses = stringList(deploy.licenseFiles);
  const licenseFiles = rawLicenses?.map(relativePath);
  if (!licenseFiles || licenseFiles.some((p) => p === undefined)) {
    throw new DeployPlanError(
      `${name}: deploy.licenseFiles must be a nonempty list of different file paths relative to the repository root`,
    );
  }
  const taken = new Set([
    ...reservedAssetNames,
    ...assetNames(deploy.binary, platforms),
  ]);
  for (const file of licenseFiles) {
    const base = posix.basename(file);
    if (taken.has(base)) {
      throw new DeployPlanError(
        `${name}: deploy.licenseFiles has a second release asset named ${base}`,
      );
    }
    taken.add(base);
  }
  let manifest = { objectKey: "", publicUrl: "" };
  if (deploy.updateManifest !== undefined) {
    const { objectKey, publicUrl } = deploy.updateManifest ?? {};
    if (typeof objectKey !== "string" || !manifestObjectKey.test(objectKey)) {
      throw new DeployPlanError(
        `${name}: deploy.updateManifest.objectKey must be a .json object key matching ${manifestObjectKey}`,
      );
    }
    let url;
    try {
      url = new URL(publicUrl);
    } catch {
      url = undefined;
    }
    if (
      typeof publicUrl !== "string" ||
      url?.protocol !== "https:" ||
      url.href !== publicUrl ||
      url.username !== "" ||
      url.password !== "" ||
      url.pathname !== `/${objectKey}` ||
      url.search !== "" ||
      url.hash !== "" ||
      // A URL parser drops a bare `?` or `#` delimiter, so it leaves `search`
      // and `hash` empty. Compare the exact canonical string to reject them.
      publicUrl !== `https://${url.host}/${objectKey}`
    ) {
      throw new DeployPlanError(
        `${name}: deploy.updateManifest.publicUrl must be https://<host>/${objectKey}`,
      );
    }
    manifest = { objectKey, publicUrl };
  }
  return {
    binary: deploy.binary,
    platforms,
    cgoPlatforms: cgo,
    versionVariable,
    entitlements,
    licenseFiles,
    manifestObjectKey: manifest.objectKey,
    manifestPublicUrl: manifest.publicUrl,
  };
}

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

// The release script tags each release `<name>@<version>`, and Git rejects a
// reference name that contains `..`, so a deployable name must not contain two
// consecutive dots. A trailing dot stays valid, because the version follows it.
const deployableName = /^(?!.*\.\.)[a-z0-9][a-z0-9._-]*$/;

// The release step tags every release whose version advances as
// `<name>@<version>`, deployable or not, so the name of any package that can be
// released must form a valid Git ref. Git rejects a ref that contains `..`,
// `@{`, an ASCII control character, a space, or one of `~ ^ : ? * [ \`, that
// has an empty path component, or that has a component starting with `.` or,
// except the last one, ending with `.lock`. `git tag` also reads a leading `-`
// as an option. The version is a Changesets semver, so only the name can break
// these rules. This mirrors `git check-ref-format refs/tags/<name>@<version>`
// without a Git dependency in the planner.
const forbiddenTagCharacters = /[\u0000- \u007f~^:?*[\\]/;

// Throws when `<name>@<version>` cannot be a Git tag. `version` is only used
// in the diagnostic, so `check()` passes the manifest version or a placeholder.
function checkReleaseTagName(name, version) {
  const components = name.split("/");
  const last = components.length - 1;
  const valid =
    !forbiddenTagCharacters.test(name) &&
    !name.startsWith("-") &&
    !name.includes("..") &&
    !name.includes("@{") &&
    !name.startsWith("/") &&
    !name.includes("//") &&
    components.every(
      (component, index) =>
        !component.startsWith(".") &&
        (index === last || !component.endsWith(".lock")),
    );
  if (!valid) {
    throw new DeployPlanError(
      `${name}: a release name must form a valid Git tag, but ${JSON.stringify(
        `${name}@${version}`,
      )} cannot be one`,
    );
  }
}

// The deploy workflow builds the image of a Droplet deployable with
// `docker build --tag <name>:<sha>`, so the package name is a container image
// repository name: lowercase alphanumeric runs joined by one separator, where
// a separator is `.`, `_`, `__` or one or more `-`. This is the repository
// path component grammar of the OCI distribution specification. A static-site
// deployable has no image and keeps the looser `deployableName` rule.
const dropletImageName = /^[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*$/;

// A Droplet service name is a container name, a Docker network alias that
// the reverse proxy resolves, and a file name on the Droplet, so it allows no
// dots and no other characters that a shell, DNS or a path would interpret.
const dropletServiceName = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/;

// A fully qualified host name of lowercase DNS labels. The reverse proxy
// requests a certificate for it, so it must have at least two labels.
const dropletHostname =
  /^(?=.{1,253}$)[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$/;

// A Cloudflare Pages project name is passed to `wrangler pages deploy
// --project-name`. The provider accepts only lowercase letters, digits and
// hyphens, beginning and ending with a letter or digit, so the planner rejects
// other names before any build or upload. It sets no length limit here.
const pagesProjectName = /^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/;

// `deploy.buildEnv` maps build-time variables of a static site to GitHub
// repository variables. The site build runs third-party build plugins without
// secrets, and only `PUBLIC_` variables are inlined into a public bundle, so
// the field may set only `PUBLIC_` names; it cannot change `PATH`,
// `NODE_OPTIONS` or another variable that controls the build tools.
const buildEnvName = /^PUBLIC_[A-Z0-9_]+$/;

// A GitHub configuration variable name: letters, digits and underscores, not
// starting with a digit or the reserved `GITHUB_` prefix. GitHub stores the
// names in upper case, so the manifest uses upper case too.
const repositoryVariableName = /^(?!GITHUB_)[A-Z_][A-Z0-9_]*$/;

// The container listens on this port inside its own network namespace. Ports
// below 1024 need privileges that the non-root server images do not have.
const minimumDropletPort = 1024;
const maximumDropletPort = 65535;

// Stable identifiers of the `deploy.upstreamProtocol` manifest field: the
// protocol that the reverse proxy uses toward the container. `h2c` (HTTP/2
// without TLS) is for servers whose bidirectional streams need HTTP/2 from end
// to end; every other server keeps the default `http1`.
export const UpstreamProtocol = Object.freeze({
  Http1: "http1",
  H2c: "h2c",
});

// `droplet-deploy.sh` receives the UDP host ports of a server as one argument:
// a comma-separated list, or this value when the server publishes none.
export const noUdpPorts = "none";

export class DeployPlanError extends Error {}

function deployable(pkg) {
  const { name, deploy } = pkg.manifest;
  if (deploy === undefined) return undefined;
  // A manifest can omit the name or give a non-string one. Report the package
  // directory, because there is no usable name to report.
  if (typeof name !== "string") {
    throw new DeployPlanError(
      `${pkg.dir}: a deployable package name must be a string, got ${
        JSON.stringify(name) ?? "no name"
      }`,
    );
  }
  const rule = targetRules.get(deploy?.target);
  if (!rule) {
    throw new DeployPlanError(
      `${name}: deploy.target must be one of ${Object.values(DeployTarget).join(", ")}`,
    );
  }
  // The workflow uses the package name in file paths, artifact names and
  // tags, so a deployable name must not contain a scope or a slash, and the
  // release tag excludes consecutive dots (see `deployableName`).
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
  const needs = deploy.needs ?? [];
  if (
    !Array.isArray(needs) ||
    (deploy.needs !== undefined && needs.length === 0) ||
    !needs.every((n) => typeof n === "string" && n !== "") ||
    new Set(needs).size !== needs.length
  ) {
    throw new DeployPlanError(
      `${name}: deploy.needs must be a nonempty list of different deployable package names`,
    );
  }
  const base = { name, dir: pkg.dir, kind: rule.kind, target: deploy.target };
  // Only a static site build reads `deploy.buildEnv`: a server reads its
  // configuration from its env file on the Droplet, and a binary build takes
  // no repository variables.
  if (
    deploy.target !== DeployTarget.CloudflarePages &&
    deploy.buildEnv !== undefined
  ) {
    throw new DeployPlanError(
      `${name}: deploy.buildEnv is allowed only for the ${DeployTarget.CloudflarePages} target`,
    );
  }
  if (deploy.target === DeployTarget.GithubRelease) {
    return { ...base, ...binaryFields(name, deploy, pkg.dir) };
  }
  if (deploy.target === DeployTarget.Droplet) {
    if (!dropletImageName.test(name)) {
      throw new DeployPlanError(
        `${name}: a droplet deployable package name must be a container image repository name matching ${dropletImageName}`,
      );
    }
    if (typeof deploy.service !== "string" || deploy.service === "") {
      throw new DeployPlanError(`${name}: deploy.service is required`);
    }
    if (!dropletServiceName.test(deploy.service)) {
      throw new DeployPlanError(
        `${name}: deploy.service must match ${dropletServiceName}`,
      );
    }
    if (typeof deploy.hostname !== "string" || deploy.hostname === "") {
      throw new DeployPlanError(`${name}: deploy.hostname is required`);
    }
    if (!dropletHostname.test(deploy.hostname)) {
      throw new DeployPlanError(
        `${name}: deploy.hostname must be a lowercase fully qualified host name`,
      );
    }
    if (
      !Number.isInteger(deploy.port) ||
      deploy.port < minimumDropletPort ||
      deploy.port > maximumDropletPort
    ) {
      throw new DeployPlanError(
        `${name}: deploy.port must be an integer from ${minimumDropletPort} to ${maximumDropletPort}`,
      );
    }
    const upstreamProtocol = deploy.upstreamProtocol ?? UpstreamProtocol.Http1;
    if (!Object.values(UpstreamProtocol).includes(upstreamProtocol)) {
      throw new DeployPlanError(
        `${name}: deploy.upstreamProtocol must be one of ${Object.values(UpstreamProtocol).join(", ")}`,
      );
    }
    const udpPorts = deploy.udpPorts ?? [];
    // The Droplet publishes each UDP port on the same host port, so ports
    // below 1024 are excluded like the container port (UDP 443 belongs to
    // the reverse proxy).
    if (
      !Array.isArray(udpPorts) ||
      (deploy.udpPorts !== undefined && udpPorts.length === 0) ||
      !udpPorts.every(
        (p) =>
          Number.isInteger(p) &&
          p >= minimumDropletPort &&
          p <= maximumDropletPort,
      ) ||
      new Set(udpPorts).size !== udpPorts.length
    ) {
      throw new DeployPlanError(
        `${name}: deploy.udpPorts must be a nonempty list of different integers from ${minimumDropletPort} to ${maximumDropletPort}`,
      );
    }
    return {
      ...base,
      service: deploy.service,
      hostname: deploy.hostname,
      // Strings, so that the deploy loop can require nonempty string fields.
      port: String(deploy.port),
      upstreamProtocol,
      udpPorts: udpPorts.length > 0 ? udpPorts.join(",") : noUdpPorts,
    };
  }
  if (typeof deploy.project !== "string" || deploy.project === "") {
    throw new DeployPlanError(`${name}: deploy.project is required`);
  }
  if (!pagesProjectName.test(deploy.project)) {
    throw new DeployPlanError(
      `${name}: deploy.project must contain only lowercase letters, digits and hyphens, and must begin and end with a letter or digit`,
    );
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
  // The deploy workflow carries the output through tab-separated and
  // line-separated transports (`jq @tsv` and `mapfile`), which corrupt tabs,
  // line breaks, other control characters and backslashes.
  if (/[\u0000-\u001f\u007f\\]/.test(output)) {
    throw new DeployPlanError(
      `${name}: deploy.output must not contain control characters or a backslash`,
    );
  }
  return {
    ...base,
    project: deploy.project,
    output: `${pkg.dir}/${output}`,
    buildEnv: checkBuildEnv(name, deploy.buildEnv),
  };
}

// Returns the `deploy.buildEnv` map of a static site, or an empty map when
// the field is not set.
function checkBuildEnv(name, buildEnv) {
  if (buildEnv === undefined) return {};
  if (
    buildEnv === null ||
    typeof buildEnv !== "object" ||
    Array.isArray(buildEnv) ||
    Object.keys(buildEnv).length === 0
  ) {
    throw new DeployPlanError(
      `${name}: deploy.buildEnv must be a nonempty map of build-time variable names to GitHub repository variable names`,
    );
  }
  for (const [variable, source] of Object.entries(buildEnv)) {
    if (!buildEnvName.test(variable)) {
      throw new DeployPlanError(
        `${name}: deploy.buildEnv key ${JSON.stringify(variable)} must match ${buildEnvName}`,
      );
    }
    if (typeof source !== "string" || !repositoryVariableName.test(source)) {
      throw new DeployPlanError(
        `${name}: deploy.buildEnv.${variable} must be a GitHub repository variable name matching ${repositoryVariableName}`,
      );
    }
  }
  return { ...buildEnv };
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

// Orders the selected names so that every workspace dependency and every
// deployable in `deploy.needs`, direct or through unselected packages, comes
// before its dependents. Among the packages whose dependencies are already
// ordered, the alphabetically first one comes next, so the result is stable.
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
        `dependency cycle: ${[...path, name].join(" -> ")}`,
      );
    }
    state.set(name, "visiting");
    const { manifest, dir } = packagesByName.get(name);
    const deps = new Set([
      ...dependencyFields.flatMap((field) =>
        Object.entries(manifest[field] ?? {}).map(([key, spec]) =>
          workspaceTarget(key, spec, dir, packagesByName, packagesByDir),
        ),
      ),
      // `checkNeeds` has validated these names.
      ...(manifest.deploy?.needs ?? []),
    ]);
    const reached = new Set();
    for (const dep of [...deps].sort()) {
      if (dep === undefined || !packagesByName.has(dep)) continue;
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
  const packages = readWorkspacePackages(repoRoot);
  plan({ changesets: [], releases: [] }, packages);
  // The release workflow ships these files, so a missing one would fail a
  // release only after the version tags exist.
  for (const pkg of packages) {
    const d = deployable(pkg);
    if (d?.target !== DeployTarget.GithubRelease) continue;
    // The release script ships a license file only as a regular file that is
    // not a symlink, so a directory or a symlink there fails the release
    // after the tags exist. `codesign` reads the entitlements file by path, so
    // it may be a symlink to a regular file.
    const inputs = [
      ...d.licenseFiles.map((file) => ({ file, followSymlinks: false })),
      ...(d.entitlements
        ? [{ file: d.entitlements, followSymlinks: true }]
        : []),
    ];
    for (const { file, followSymlinks } of inputs) {
      const path = join(repoRoot, file);
      let stat;
      try {
        stat = followSymlinks ? statSync(path) : lstatSync(path);
      } catch (error) {
        if (error?.code === "ENOENT" || error?.code === "ENOTDIR") {
          // A dangling entitlements symlink is also a missing file.
          throw new DeployPlanError(`${d.name}: ${file} does not exist`);
        }
        throw error;
      }
      if (!stat.isFile()) {
        throw new DeployPlanError(
          `${d.name}: ${file} is not a regular file${
            followSymlinks ? "" : " (a directory or a symlink is not allowed)"
          }`,
        );
      }
    }
  }
  // The plan above has no releases, so it cannot see a release name. Check the
  // name of every workspace package here, so the pull request that adds or
  // renames a package fails before a later release could. A deployable with a
  // non-string name has already failed in the plan; another package with no
  // usable name is not in scope, so it is skipped.
  for (const { manifest } of packages) {
    if (typeof manifest.name !== "string") continue;
    checkReleaseTagName(
      manifest.name,
      typeof manifest.version === "string" ? manifest.version : "<version>",
    );
  }
  return ids;
}

// Two deployables that name the same Droplet service or Pages project would
// overwrite each other, while both get a release tag. Two Droplet services
// with the same host name would replace each other's reverse proxy route, and
// two that publish the same UDP port could not run at the same time.
// Checks every deployable of the workspace, selected or not.
function rejectSharedDestinations(packages) {
  const owners = new Map();
  for (const pkg of packages) {
    const d = deployable(pkg);
    if (!d) continue;
    const keys = [`${d.target}:${d.service ?? d.project ?? d.binary}`];
    if (d.manifestObjectKey)
      keys.push(`${d.target}-manifest:${d.manifestObjectKey}`);
    if (d.hostname !== undefined)
      keys.push(`${d.target}-hostname:${d.hostname}`);
    if (d.udpPorts !== undefined && d.udpPorts !== noUdpPorts) {
      for (const port of d.udpPorts.split(","))
        keys.push(`${d.target}-udp:${port}`);
    }
    for (const key of keys) {
      const owner = owners.get(key);
      if (owner) {
        throw new DeployPlanError(
          `${owner} and ${d.name} both deploy to ${key}`,
        );
      }
      owners.set(key, d.name);
    }
  }
}

// `deploy.needs` names the deployables that must deploy before this one when
// a run deploys both. A deployable that the run does not select is already
// deployed, so `needs` only orders and never selects. Servers deploy before
// static sites, so a server cannot need a static site. Checks every
// deployable of the workspace, selected or not, including cycles.
function checkNeeds(packages, packagesByName) {
  const kinds = new Map();
  for (const pkg of packages) {
    const d = deployable(pkg);
    if (d) kinds.set(d.name, d.kind);
  }
  for (const [name, kind] of kinds) {
    for (const need of packagesByName.get(name).manifest.deploy.needs ?? []) {
      if (need === name) {
        throw new DeployPlanError(`${name}: deploy.needs must not name itself`);
      }
      if (!kinds.has(need)) {
        throw new DeployPlanError(
          `${name}: deploy.needs names ${need}, which is not a deployable workspace package`,
        );
      }
      if (kindOrder.indexOf(kinds.get(need)) > kindOrder.indexOf(kind)) {
        throw new DeployPlanError(
          `${name}: deploy.needs names ${need}, a ${kinds.get(need)} that deploys after every ${kind}`,
        );
      }
    }
  }
  topologicalOrder(new Set(kinds.keys()), packagesByName);
}

// Builds the deployment plan from the `changeset status` JSON and the
// workspace packages.
export function plan(status, packages) {
  // Only named packages can be released, depended on or ordered. A package
  // without a usable name is still passed whole to the destination checks, so
  // a nameless deployable keeps its own diagnostic.
  const packagesByName = new Map(
    packages
      .filter(
        ({ manifest }) =>
          typeof manifest.name === "string" && manifest.name !== "",
      )
      .map((pkg) => [pkg.manifest.name, pkg]),
  );
  const statusReleases = status.releases ?? [];
  const allReleases = statusReleases.map(({ name, type, newVersion }) => {
    if (!packagesByName.has(name)) {
      throw new DeployPlanError(`release ${name} is not a workspace package`);
    }
    return { name, type, version: newVersion };
  });
  // A `none` release keeps its current version, whose tag may already exist.
  const releases = allReleases
    .filter((r) => r.type !== ReleaseType.None)
    .map(({ name, version }) => ({ name, version }));
  const versions = new Map(allReleases.map((r) => [r.name, r.version]));
  rejectSharedDestinations(packages);
  checkNeeds(packages, packagesByName);
  const types = new Map(allReleases.map((r) => [r.name, r.type]));
  const byName = new Map(
    [...versions.keys()]
      .map((name) => deployable(packagesByName.get(name)))
      .filter(Boolean)
      // A binary release is published under its version tag, so a `none`
      // release, which keeps its version and gets no tag, publishes nothing.
      .filter(
        (d) =>
          d.kind !== DeployKind.Binary ||
          types.get(d.name) !== ReleaseType.None,
      )
      .map((d) => [d.name, { ...d, version: versions.get(d.name) }]),
  );
  for (const d of byName.values()) {
    if (d.kind === DeployKind.Binary && !binaryReleaseVersion.test(d.version)) {
      throw new DeployPlanError(
        `${d.name}: a binary release version must be MAJOR.MINOR.PATCH, not ${d.version}`,
      );
    }
  }
  // After the deployable map, so a deployable that fails its own name rule
  // keeps that diagnostic. A `none` release is checked too, because a later
  // run can advance the same package and tag it.
  for (const [name, version] of versions) checkReleaseTagName(name, version);
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
  const changesets = status.changesets ?? [];
  // Changesets CLI 3.0.3 records the source IDs of every non-skipped release,
  // including `none` releases, after applying its config and package rules.
  // It also consumes empty changesets, which contribute to no release.
  const contributingChangesetIds = new Set(
    statusReleases.flatMap(({ changesets: ids }) => ids.map(checkChangesetId)),
  );
  return {
    // The deploy jobs check that these changeset files are still pending on
    // `main` before they upload, so a rerun of an old run cannot deploy a
    // superseded revision.
    changesets: changesets.map(({ id }) => checkChangesetId(id)),
    consumedChangesets: changesets
      .filter(
        ({ id, releases }) =>
          releases.length === 0 || contributingChangesetIds.has(id),
      )
      .map(({ id }) => id),
    releases: releases.sort((a, b) => a.name.localeCompare(b.name)),
    servers: deployables.filter((d) => d.kind === DeployKind.Server),
    staticSites: deployables.filter((d) => d.kind === DeployKind.StaticSite),
    binaries: deployables.filter((d) => d.kind === DeployKind.Binary),
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
      consumedChangesets: result.consumedChangesets,
      releases: result.releases,
      servers: result.servers.map((d) => d.name),
      staticSites: result.staticSites.map((d) => d.name),
      binaries: result.binaries.map((d) => d.name),
    }),
  );
  if (env.GITHUB_OUTPUT) {
    appendFileSync(
      env.GITHUB_OUTPUT,
      [
        `releases=${JSON.stringify(result.releases)}`,
        `servers=${JSON.stringify(result.servers)}`,
        `static_sites=${JSON.stringify(result.staticSites)}`,
        `binaries=${JSON.stringify(result.binaries)}`,
        // Run the release job for any pending changeset, including one with
        // only `none` releases.
        `changesets=${JSON.stringify(result.changesets)}`,
        `consumed_changesets=${JSON.stringify(result.consumedChangesets)}`,
        `has_changesets=${result.changesets.length > 0}`,
        `has_servers=${result.servers.length > 0}`,
        `has_static_sites=${result.staticSites.length > 0}`,
        `has_binaries=${result.binaries.length > 0}`,
        // The macOS build job runs only for a cgo platform, and the macOS
        // signing job only for a darwin platform.
        `has_cgo_binaries=${result.binaries.some((d) => d.cgoPlatforms.length > 0)}`,
        `has_darwin_binaries=${result.binaries.some((d) => d.platforms.some((p) => p.startsWith("darwin-")))}`,
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
      [...result.servers, ...result.staticSites, ...result.binaries].forEach(
        (d, i) => {
          lines.push(
            `| ${i + 1} | ${d.name} | ${d.version} | ${d.kind} | ${d.target} |`,
          );
        },
      );
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
