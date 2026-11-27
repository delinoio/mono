import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import * as fs from "node:fs";
import { join } from "node:path";

const root = execFileSync("git", ["rev-parse", "--show-toplevel"], {
  encoding: "utf8",
}).trim();
const tmp = join(root, ".t3-tmp");
fs.mkdirSync(tmp, { recursive: true });
const work = fs.mkdtempSync(join(tmp, "image-cli-"));
const env = Object.fromEntries(
  Object.entries(process.env).filter(
    ([key]) => !key.startsWith("OWNSAND_AGENT_"),
  ),
);
Object.assign(env, { TMPDIR: tmp, GOTMPDIR: tmp, TMP: tmp, TEMP: tmp });

const baseline = process.argv[2];
if (baseline && process.platform !== "linux")
  throw new Error("Run the baseline countercheck on Linux.");
const rel = "cmds/ownsand-agent/internal/imagebuild/imagebuild.go";
const publisher = "cmds/ownsand-agent/internal/images/images.go";
const read = (path) =>
  baseline
    ? execFileSync("git", ["show", baseline + ":" + path], {
        cwd: root,
        encoding: "utf8",
      })
    : fs.readFileSync(join(root, path), "utf8");

const buildPath = join(work, "imagebuild.go");
const build = read(rel);
const anchor = '\n\tif opts.ImagesDir == "" {';
assert.equal(build.split(anchor).length, 2);
const installArguments = baseline ? "ctx, dir" : "ctx, dir, opts.GuestOS";
fs.writeFileSync(
  buildPath,
  build.replace(
    anchor,
    `
	// Probe entry point: use staged marker files instead of starting a VM.
	if os.Getenv("OWNSAND_VALIDATION_STAGED_IMAGE") == "1" {
		dir, err := images.Dir(opts.ImagesDir, opts.GuestOS)
		if err != nil {
			return err
		}
		return withBuildLock(dir, opts.Logger, func() error {
			return images.Install(${installArguments})
		})
	}
` + anchor,
  ),
);
const replace = { [join(root, rel)]: buildPath };
if (baseline) {
  const publisherPath = join(work, "images.go");
  fs.writeFileSync(publisherPath, read(publisher));
  replace[join(root, publisher)] = publisherPath;
}
const overlay = join(work, "overlay.json");
fs.writeFileSync(overlay, JSON.stringify({ Replace: replace }));
const agent = join(
  work,
  process.platform === "win32" ? "ownsand-agent.exe" : "ownsand-agent",
);
execFileSync(
  "go",
  [
    "build",
    "-mod=readonly",
    "-overlay",
    overlay,
    "-o",
    agent,
    "./cmds/ownsand-agent",
  ],
  {
    cwd: root,
    env,
    stdio: "inherit",
  },
);
if (process.platform === "darwin") {
  execFileSync(
    "codesign",
    [
      "--force",
      "--sign",
      "-",
      "--entitlements",
      join(root, "cmds/ownsand-agent/entitlements.plist"),
      agent,
    ],
    { stdio: "inherit" },
  );
}

const layouts = {
  macos: {
    os: 2,
    files: ["disk.img", "aux.img", "hardware-model"],
    omitted: "aux.img",
  },
  windows: { os: 3, files: ["disk.vhdx"], omitted: "disk.vhdx" },
};
function generation(dir, guest, marker) {
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(
    join(dir, "image.json"),
    JSON.stringify({ guest_os: layouts[guest].os, source: marker }),
  );
  for (const file of layouts[guest].files)
    fs.writeFileSync(join(dir, file), marker);
}
function expectGeneration(dir, guest, marker) {
  const metadata = JSON.parse(fs.readFileSync(join(dir, "image.json"), "utf8"));
  assert.deepEqual(metadata, { guest_os: layouts[guest].os, source: marker });
  for (const file of layouts[guest].files)
    assert.equal(fs.readFileSync(join(dir, file), "utf8"), marker);
}
function run(guest, scenario) {
  const state = join(work, guest, scenario);
  const canonical = join(state, "images", guest);
  const stage = canonical + ".build";
  const backup = canonical + ".old";
  if (scenario === "recover") generation(backup, guest, "previous");
  else if (scenario !== "first build") generation(canonical, guest, "previous");
  if (scenario === "wrong OS")
    generation(stage, guest === "macos" ? "windows" : "macos", "wrong OS");
  else generation(stage, guest, "next");
  let diagnostic = layouts[guest].omitted;
  if (scenario === "malformed metadata") {
    fs.writeFileSync(join(stage, "image.json"), "{");
    diagnostic = "parse image.json";
  } else if (scenario === "wrong OS") {
    diagnostic =
      guest === "macos" ? "want GUEST_OS_MACOS" : "want GUEST_OS_WINDOWS";
  } else if (scenario !== "replace") {
    fs.rmSync(join(stage, layouts[guest].omitted));
  }

  const proc = spawnSync(
    agent,
    ["image", "build", "--no-color", "--state-dir", state, "--os", guest],
    {
      cwd: root,
      env: { ...env, OWNSAND_VALIDATION_STAGED_IMAGE: "1" },
      encoding: "utf8",
    },
  );
  if (proc.error) throw proc.error;
  const result = {
    status: proc.status,
    stdout: proc.stdout,
    stderr: proc.stderr,
  };
  const succeed = scenario === "replace" || baseline;
  assert.equal(proc.status, succeed ? 0 : 1, JSON.stringify(result));
  if (succeed) {
    assert.match(proc.stdout, /^Built /m);
    assert.equal(fs.existsSync(stage), false);
    if (baseline) {
      const metadata = JSON.parse(
        fs.readFileSync(join(canonical, "image.json"), "utf8"),
      );
      assert.equal(metadata.source, "next");
      assert.equal(
        fs.existsSync(join(canonical, layouts[guest].omitted)),
        false,
      );
    } else {
      expectGeneration(canonical, guest, "next");
    }
  } else {
    assert.doesNotMatch(proc.stdout, /^Built /m);
    assert.match(proc.stderr, /image build failed:.*validate staged image/);
    assert.ok(proc.stderr.includes(diagnostic), JSON.stringify(result));
    assert.equal(fs.existsSync(stage), true);
    if (scenario === "first build")
      assert.equal(fs.existsSync(canonical), false);
    else expectGeneration(canonical, guest, "previous");
  }
  assert.equal(fs.existsSync(backup), false);
  console.log(
    guest,
    scenario,
    baseline ? "REPRODUCED_INCOMPLETE_PUBLICATION" : "PASS",
  );
}
for (const guest of Object.keys(layouts)) {
  const cases = baseline
    ? ["missing file"]
    : [
        "missing file",
        "malformed metadata",
        "wrong OS",
        "recover",
        "first build",
        "replace",
      ];
  for (const scenario of cases) run(guest, scenario);
}
