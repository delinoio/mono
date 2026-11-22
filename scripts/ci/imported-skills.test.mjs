// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import {
  cpSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import {
  buildManifest,
  LockFile,
  ManifestFile,
  ProblemKind,
  SkillsDirectory,
  verifyImportedSkills,
} from "./imported-skills.mjs";

const repositoryRoot = join(dirname(fileURLToPath(import.meta.url)), "../..");
const script = join(repositoryRoot, "scripts/ci/imported-skills.mjs");

const commit = "a".repeat(40);

function lockEntry(overrides = {}) {
  return {
    source: "example/skills",
    commit,
    sourceType: "github",
    skillPath: "skills/demo/SKILL.md",
    computedHash: "b".repeat(64),
    installMode: "copy",
    ...overrides,
  };
}

function notice(entry) {
  return [
    "This directory contains an agent skill imported from an upstream project.",
    "",
    `Source repository: https://github.com/${entry.source}`,
    `Skill path: ${entry.skillPath}`,
    `Commit: ${entry.commit}`,
    "",
  ].join("\n");
}

// A scratch repository tree with one skill that has a NOTICE, one skill that
// has a LICENSE, and a manifest generated from them.
function fixture(t) {
  const root = mkdtempSync(join(tmpdir(), "imported-skills-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const withNotice = lockEntry();
  const withLicense = lockEntry({
    skillPath: "skills/licensed/SKILL.md",
    computedHash: "c".repeat(64),
  });
  const write = (path, content) => {
    mkdirSync(dirname(join(root, path)), { recursive: true });
    writeFileSync(join(root, path), content);
  };
  write(
    LockFile,
    `${JSON.stringify({ version: 2, skills: { demo: withNotice, licensed: withLicense } }, null, 2)}\n`,
  );
  write(`${SkillsDirectory}/demo/SKILL.md`, "# Demo\n");
  write(`${SkillsDirectory}/demo/rules/one.md`, "one\n");
  write(`${SkillsDirectory}/demo/NOTICE`, notice(withNotice));
  write(`${SkillsDirectory}/licensed/SKILL.md`, "# Licensed\n");
  write(`${SkillsDirectory}/licensed/LICENSE`, "MIT License\n");
  write(ManifestFile, `${JSON.stringify(buildManifest(root), null, 2)}\n`);
  return {
    root,
    write,
    remove: (path) => rmSync(join(root, path), { force: true }),
    read: (path) => readFileSync(join(root, path), "utf8"),
  };
}

function kinds(root) {
  return verifyImportedSkills(root).map((problem) => problem.kind);
}

test("a consistent fixture has no problem", (t) => {
  const f = fixture(t);
  assert.deepEqual(verifyImportedSkills(f.root), []);
});

test("the checked-in skills match their lock entries and manifest", () => {
  assert.deepEqual(verifyImportedSkills(repositoryRoot), []);
});

test("the manifest of the checked-in skills is the one that --write-manifest builds", () => {
  const manifest = buildManifest(repositoryRoot);
  assert.deepEqual(
    JSON.parse(readFileSync(join(repositoryRoot, ManifestFile), "utf8")),
    manifest,
  );
});

test("a missing NOTICE is reported", (t) => {
  const f = fixture(t);
  f.remove(`${SkillsDirectory}/demo/NOTICE`);
  const found = kinds(f.root);
  assert.ok(found.includes(ProblemKind.LicenseMissing));
  assert.ok(found.includes(ProblemKind.FileMissing));
});

test("a missing LICENSE is reported", (t) => {
  const f = fixture(t);
  f.remove(`${SkillsDirectory}/licensed/LICENSE`);
  const found = kinds(f.root);
  assert.ok(found.includes(ProblemKind.LicenseMissing));
});

test("a NOTICE that differs from the lock entry is reported", (t) => {
  const f = fixture(t);
  f.write(
    `${SkillsDirectory}/demo/NOTICE`,
    notice(lockEntry({ commit: "d".repeat(40) })),
  );
  const problems = verifyImportedSkills(f.root);
  assert.ok(
    problems.some(
      (problem) =>
        problem.kind === ProblemKind.NoticeMismatch &&
        problem.message.includes(`Commit: ${commit}`),
    ),
  );
});

test("a NOTICE with CRLF line endings still matches", (t) => {
  const f = fixture(t);
  const crlf = notice(lockEntry()).replaceAll("\n", "\r\n");
  f.write(`${SkillsDirectory}/demo/NOTICE`, crlf);
  assert.deepEqual(verifyImportedSkills(f.root), []);
});

test("an edited skill file is reported with its path", (t) => {
  const f = fixture(t);
  f.write(`${SkillsDirectory}/demo/rules/one.md`, "changed\n");
  const problems = verifyImportedSkills(f.root);
  assert.equal(problems.length, 1);
  assert.equal(problems[0].kind, ProblemKind.FileChanged);
  assert.equal(problems[0].path, "rules/one.md");
});

test("a line-ending-only difference is not a change", (t) => {
  const f = fixture(t);
  f.write(`${SkillsDirectory}/demo/rules/one.md`, "one\r\n");
  assert.deepEqual(verifyImportedSkills(f.root), []);
});

test("an added and a removed skill file are reported", (t) => {
  const f = fixture(t);
  f.write(`${SkillsDirectory}/demo/rules/extra.md`, "extra\n");
  f.remove(`${SkillsDirectory}/demo/rules/one.md`);
  const found = kinds(f.root);
  assert.ok(found.includes(ProblemKind.FileUnexpected));
  assert.ok(found.includes(ProblemKind.FileMissing));
});

test("a missing skill directory is reported", (t) => {
  const f = fixture(t);
  rmSync(join(f.root, SkillsDirectory, "licensed"), { recursive: true });
  assert.deepEqual(kinds(f.root), [ProblemKind.SkillDirectoryMissing]);
});

test("a missing SKILL.md is reported", (t) => {
  const f = fixture(t);
  f.remove(`${SkillsDirectory}/licensed/SKILL.md`);
  const found = kinds(f.root);
  assert.ok(found.includes(ProblemKind.SkillFileMissing));
});

test("a hand-edited lock entry is reported", (t) => {
  const f = fixture(t);
  const lock = JSON.parse(f.read(LockFile));
  lock.skills.demo.computedHash = "e".repeat(64);
  f.write(LockFile, `${JSON.stringify(lock, null, 2)}\n`);
  const problems = verifyImportedSkills(f.root);
  assert.equal(problems.length, 1);
  assert.equal(problems[0].kind, ProblemKind.LockEntryChanged);
  assert.match(problems[0].message, /computedHash/u);
});

test("a malformed lock entry is reported", (t) => {
  const f = fixture(t);
  const lock = JSON.parse(f.read(LockFile));
  lock.skills.demo.commit = "main";
  f.write(LockFile, `${JSON.stringify(lock, null, 2)}\n`);
  assert.ok(kinds(f.root).includes(ProblemKind.LockEntryInvalid));
});

test("a skill that the manifest lacks, and one that the lock lacks, are reported", (t) => {
  const f = fixture(t);
  const manifest = JSON.parse(f.read(ManifestFile));
  manifest.skills.ghost = manifest.skills.demo;
  delete manifest.skills.licensed;
  f.write(ManifestFile, `${JSON.stringify(manifest, null, 2)}\n`);
  const found = kinds(f.root);
  assert.ok(found.includes(ProblemKind.ManifestUnknownSkill));
  assert.ok(found.includes(ProblemKind.ManifestMissingSkill));
});

test("a missing lock file and a missing manifest are reported", (t) => {
  const f = fixture(t);
  f.remove(ManifestFile);
  assert.deepEqual(kinds(f.root), [ProblemKind.ManifestMissing]);
  f.remove(LockFile);
  assert.deepEqual(kinds(f.root), [ProblemKind.LockMissing]);
});

test("the check never writes the lock file or the manifest", (t) => {
  const f = fixture(t);
  f.write(`${SkillsDirectory}/demo/rules/one.md`, "changed\n");
  const lock = f.read(LockFile);
  const manifest = f.read(ManifestFile);
  const result = spawnSync(process.execPath, [script, "--root", f.root], {
    encoding: "utf8",
  });
  assert.equal(result.status, 1);
  assert.equal(f.read(LockFile), lock);
  assert.equal(f.read(ManifestFile), manifest);
});

test("the command line reports problems and exits with status 1", (t) => {
  const f = fixture(t);
  f.remove(`${SkillsDirectory}/demo/NOTICE`);
  const result = spawnSync(process.execPath, [script, "--root", f.root], {
    encoding: "utf8",
  });
  assert.equal(result.status, 1);
  assert.match(result.stderr, /imported_skills_problem kind=license-missing/u);
  assert.match(result.stderr, /imported_skills_failed problems=2/u);
});

test("the command line passes on a consistent tree and on the repository", (t) => {
  const f = fixture(t);
  for (const root of [f.root, repositoryRoot]) {
    const result = spawnSync(process.execPath, [script, "--root", root], {
      encoding: "utf8",
    });
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.stdout, "imported_skills_ok\n");
  }
});

test("--write-manifest records a deliberate correction and the check then passes", (t) => {
  const f = fixture(t);
  f.write(`${SkillsDirectory}/demo/rules/one.md`, "corrected\n");
  assert.deepEqual(kinds(f.root), [ProblemKind.FileChanged]);
  const lock = f.read(LockFile);
  const result = spawnSync(
    process.execPath,
    [script, "--root", f.root, "--write-manifest"],
    { encoding: "utf8" },
  );
  assert.equal(result.status, 0, result.stderr);
  assert.deepEqual(verifyImportedSkills(f.root), []);
  assert.equal(f.read(LockFile), lock);
});

test("an unknown argument exits with status 2", () => {
  const result = spawnSync(process.execPath, [script, "--bogus"], {
    encoding: "utf8",
  });
  assert.equal(result.status, 2);
  assert.match(result.stderr, /imported_skills_error/u);
});

test("the repository copy test reads the checked-in tree unchanged", (t) => {
  // A copy of the real skill directories and lock file passes, so the result
  // does not depend on the repository path.
  const root = mkdtempSync(join(tmpdir(), "imported-skills-copy-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  cpSync(join(repositoryRoot, SkillsDirectory), join(root, SkillsDirectory), {
    recursive: true,
  });
  cpSync(join(repositoryRoot, LockFile), join(root, LockFile));
  mkdirSync(join(root, "scripts/ci"), { recursive: true });
  cpSync(join(repositoryRoot, ManifestFile), join(root, ManifestFile));
  assert.deepEqual(verifyImportedSkills(root), []);
});
