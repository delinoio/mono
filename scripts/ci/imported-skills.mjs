// SPDX-License-Identifier: Apache-2.0
//
// Read-only check of the imported agent skills that the root `skills-lock.json`
// records. Usage:
//
//   node scripts/ci/imported-skills.mjs [--root <dir>]
//   node scripts/ci/imported-skills.mjs [--root <dir>] --write-manifest
//
// Why this check exists: the `skills` CLI pins each skill with a
// `computedHash` of the upstream directory content, but the repository keeps
// local corrections and a `NOTICE` file in these directories, so the recorded
// hash can no longer match the checked-in copy and cannot detect an unintended
// change. The checked-in copy is the restore source (Git, not a stock upstream
// install). This check therefore verifies, for every lock entry:
//
// - the lock entry is complete (source, commit, skill path, hash, copy mode);
// - the skill directory exists and holds `SKILL.md` and a `LICENSE` or
//   `NOTICE` file;
// - a `NOTICE` file names the source repository, skill path and commit exactly
//   as the lock entry records them;
// - the lock entry equals the snapshot in the manifest, so a change of the
//   lock entry (which only the CLI should write) and the manifest change
//   together;
// - every file of the directory has the SHA-256 recorded in the manifest, and
//   the directory holds no file that the manifest does not list;
// - the directory holds no symbolic link. The check never follows a link: a
//   link (relative, absolute or dangling) is an unsupported entry, because
//   neither the manifest hash nor `git` content review covers its target.
//
// The manifest `scripts/ci/imported-skills.manifest.json` is the baseline of
// the reviewed local corrections. A deliberate correction of an imported skill
// file updates the manifest in the same commit with `--write-manifest`; the
// check never writes it otherwise and never writes `skills-lock.json`.
import { createHash } from "node:crypto";
import {
  existsSync,
  readdirSync,
  readFileSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { dirname, join, relative, resolve, sep } from "node:path";
import { fileURLToPath } from "node:url";

export const SkillsDirectory = ".agents/skills";
export const LockFile = "skills-lock.json";
export const ManifestFile = "scripts/ci/imported-skills.manifest.json";

// Stable identifiers of the problems that the check reports.
export const ProblemKind = Object.freeze({
  LockMissing: "lock-missing",
  LockInvalid: "lock-invalid",
  LockEntryInvalid: "lock-entry-invalid",
  ManifestMissing: "manifest-missing",
  ManifestInvalid: "manifest-invalid",
  ManifestUnknownSkill: "manifest-unknown-skill",
  ManifestMissingSkill: "manifest-missing-skill",
  LockEntryChanged: "lock-entry-changed",
  SkillDirectoryMissing: "skill-directory-missing",
  SkillFileMissing: "skill-file-missing",
  LicenseMissing: "license-missing",
  NoticeMismatch: "notice-mismatch",
  FileMissing: "file-missing",
  FileChanged: "file-changed",
  FileUnexpected: "file-unexpected",
  SymlinkUnsupported: "symlink-unsupported",
});

// The lock entry fields that the check compares with the manifest snapshot and
// the `NOTICE` file.
const lockFields = [
  "source",
  "sourceType",
  "commit",
  "skillPath",
  "computedHash",
  "installMode",
];

// The SHA-256 of the file content with CRLF line endings read as LF, so that a
// checkout with `core.autocrlf` hashes like one without it.
export function hashFile(path) {
  const content = readFileSync(path);
  const normalized = Buffer.from(
    content.toString("latin1").replaceAll("\r\n", "\n"),
    "latin1",
  );
  return createHash("sha256").update(normalized).digest("hex");
}

// Relative POSIX paths of every regular file and of every symbolic link below
// `directory`, each sorted. A `Dirent` of a symbolic link is neither a file
// nor a directory, so the walk records it separately and neither recurses into
// nor stats its target (a dangling or looping link must not fail the walk).
function listEntries(directory) {
  const files = [];
  const symlinks = [];
  const walk = (current) => {
    for (const entry of readdirSync(current, { withFileTypes: true })) {
      const path = join(current, entry.name);
      const relativePath = relative(directory, path).split(sep).join("/");
      if (entry.isSymbolicLink()) {
        symlinks.push(relativePath);
      } else if (entry.isDirectory()) {
        walk(path);
      } else if (entry.isFile()) {
        files.push(relativePath);
      }
    }
  };
  walk(directory);
  return { files: files.sort(), symlinks: symlinks.sort() };
}

function symlinkMessage(directoryPath, file) {
  return `${directoryPath}/${file} is a symbolic link, which an imported skill directory does not support; replace it with a regular file or remove it`;
}

function readJson(path) {
  return JSON.parse(readFileSync(path, "utf8"));
}

function isPlainObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

// Returns the validation message of a lock entry, or undefined when it is
// complete.
function lockEntryProblem(entry) {
  if (!isPlainObject(entry)) {
    return "the entry is not an object";
  }
  for (const field of lockFields) {
    if (typeof entry[field] !== "string" || entry[field] === "") {
      return `the field ${field} is missing or not a string`;
    }
  }
  if (!/^[0-9a-f]{40}$/u.test(entry.commit)) {
    return "the commit is not a 40-character lower-case hexadecimal SHA";
  }
  if (!/^[0-9a-f]{64}$/u.test(entry.computedHash)) {
    return "the computedHash is not a 64-character lower-case SHA-256";
  }
  if (entry.sourceType !== "github") {
    return `the sourceType ${entry.sourceType} is not github`;
  }
  if (entry.installMode !== "copy") {
    return `the installMode ${entry.installMode} is not copy`;
  }
  return undefined;
}

// Lock entry fields that a `NOTICE` file must repeat, as they appear in it.
function noticeLines(entry) {
  return [
    `Source repository: https://github.com/${entry.source}`,
    `Skill path: ${entry.skillPath}`,
    `Commit: ${entry.commit}`,
  ];
}

function snapshotOf(entry) {
  return Object.fromEntries(lockFields.map((field) => [field, entry[field]]));
}

// Builds the manifest for the checked-in skill directories. Throws on an
// invalid or missing lock file.
export function buildManifest(root) {
  const lockPath = join(root, LockFile);
  if (!existsSync(lockPath)) {
    throw new Error(`${LockFile} does not exist`);
  }
  const lock = readJson(lockPath);
  if (!isPlainObject(lock) || !isPlainObject(lock.skills)) {
    throw new Error(`${LockFile} has no skills object`);
  }
  const skills = {};
  for (const name of Object.keys(lock.skills).sort()) {
    const entry = lock.skills[name];
    const problem = lockEntryProblem(entry);
    if (problem !== undefined) {
      throw new Error(`${LockFile} entry ${name} is invalid: ${problem}`);
    }
    const directory = join(root, SkillsDirectory, name);
    if (!existsSync(directory) || !statSync(directory).isDirectory()) {
      throw new Error(`${SkillsDirectory}/${name} does not exist`);
    }
    const entries = listEntries(directory);
    if (entries.symlinks.length > 0) {
      throw new Error(
        entries.symlinks
          .map((file) => symlinkMessage(`${SkillsDirectory}/${name}`, file))
          .join("; "),
      );
    }
    const files = {};
    for (const file of entries.files) {
      files[file] = hashFile(join(directory, file));
    }
    skills[name] = { lock: snapshotOf(entry), files };
  }
  return { version: 1, skills };
}

// Returns the list of `{ kind, skill?, path?, message }` problems.
export function verifyImportedSkills(root) {
  const problems = [];
  const report = (kind, message, extra = {}) =>
    problems.push({ kind, message, ...extra });

  const lockPath = join(root, LockFile);
  if (!existsSync(lockPath)) {
    report(ProblemKind.LockMissing, `${LockFile} does not exist`);
    return problems;
  }
  let lock;
  try {
    lock = readJson(lockPath);
  } catch (error) {
    report(
      ProblemKind.LockInvalid,
      `${LockFile} is not valid JSON: ${error.message}`,
    );
    return problems;
  }
  if (!isPlainObject(lock) || !isPlainObject(lock.skills)) {
    report(ProblemKind.LockInvalid, `${LockFile} has no skills object`);
    return problems;
  }

  let manifest;
  const manifestPath = join(root, ManifestFile);
  if (!existsSync(manifestPath)) {
    report(
      ProblemKind.ManifestMissing,
      `${ManifestFile} does not exist; create it with --write-manifest`,
    );
  } else {
    try {
      manifest = readJson(manifestPath);
      if (!isPlainObject(manifest) || !isPlainObject(manifest.skills)) {
        throw new Error("no skills object");
      }
    } catch (error) {
      manifest = undefined;
      report(
        ProblemKind.ManifestInvalid,
        `${ManifestFile} is invalid: ${error.message}`,
      );
    }
  }

  for (const name of Object.keys(lock.skills).sort()) {
    const entry = lock.skills[name];
    const entryProblem = lockEntryProblem(entry);
    if (entryProblem !== undefined) {
      report(
        ProblemKind.LockEntryInvalid,
        `${LockFile} entry ${name} is invalid: ${entryProblem}`,
        { skill: name },
      );
      continue;
    }

    const directory = join(root, SkillsDirectory, name);
    const directoryPath = `${SkillsDirectory}/${name}`;
    if (!existsSync(directory) || !statSync(directory).isDirectory()) {
      report(
        ProblemKind.SkillDirectoryMissing,
        `${directoryPath} does not exist; restore it from Git`,
        { skill: name },
      );
      continue;
    }

    if (!existsSync(join(directory, "SKILL.md"))) {
      report(
        ProblemKind.SkillFileMissing,
        `${directoryPath}/SKILL.md does not exist; restore it from Git`,
        { skill: name, path: "SKILL.md" },
      );
    }

    const hasLicense = existsSync(join(directory, "LICENSE"));
    const hasNotice = existsSync(join(directory, "NOTICE"));
    if (!hasLicense && !hasNotice) {
      report(
        ProblemKind.LicenseMissing,
        `${directoryPath} has neither a LICENSE nor a NOTICE file; restore it from Git`,
        { skill: name },
      );
    }
    if (hasNotice) {
      // Line endings do not matter for the comparison.
      const notice = readFileSync(join(directory, "NOTICE"), "utf8")
        .replaceAll("\r\n", "\n")
        .split("\n");
      for (const line of noticeLines(entry)) {
        if (!notice.includes(line)) {
          report(
            ProblemKind.NoticeMismatch,
            `${directoryPath}/NOTICE does not contain the line "${line}" that ${LockFile} requires`,
            { skill: name, path: "NOTICE" },
          );
        }
      }
    }

    if (manifest === undefined) {
      continue;
    }
    const baseline = manifest.skills[name];
    if (!isPlainObject(baseline) || !isPlainObject(baseline.files)) {
      report(
        ProblemKind.ManifestMissingSkill,
        `${ManifestFile} has no entry for ${name}`,
        { skill: name },
      );
      continue;
    }
    for (const field of lockFields) {
      if (baseline.lock?.[field] !== entry[field]) {
        report(
          ProblemKind.LockEntryChanged,
          `${LockFile} entry ${name} field ${field} differs from ${ManifestFile}; only the skills CLI changes the lock entry, so reinstall the skill with the CLI, reapply the local corrections and regenerate the manifest with --write-manifest`,
          { skill: name },
        );
      }
    }
    const { files: actual, symlinks } = listEntries(directory);
    for (const file of symlinks) {
      report(
        ProblemKind.SymlinkUnsupported,
        symlinkMessage(directoryPath, file),
        { skill: name, path: file },
      );
    }
    for (const file of Object.keys(baseline.files).sort()) {
      if (!actual.includes(file)) {
        report(
          ProblemKind.FileMissing,
          `${directoryPath}/${file} is missing; restore it from Git`,
          { skill: name, path: file },
        );
      } else if (hashFile(join(directory, file)) !== baseline.files[file]) {
        report(
          ProblemKind.FileChanged,
          `${directoryPath}/${file} differs from ${ManifestFile}; restore it from Git, or run --write-manifest when the change is a deliberate local correction`,
          { skill: name, path: file },
        );
      }
    }
    for (const file of actual) {
      if (!(file in baseline.files)) {
        report(
          ProblemKind.FileUnexpected,
          `${directoryPath}/${file} is not listed in ${ManifestFile}; remove it, or run --write-manifest when it is a deliberate addition`,
          { skill: name, path: file },
        );
      }
    }
  }

  if (manifest !== undefined) {
    for (const name of Object.keys(manifest.skills).sort()) {
      if (!(name in lock.skills)) {
        report(
          ProblemKind.ManifestUnknownSkill,
          `${ManifestFile} lists ${name}, which ${LockFile} does not record`,
          { skill: name },
        );
      }
    }
  }
  return problems;
}

function parseArguments(argv) {
  const options = { root: undefined, writeManifest: false };
  for (let index = 0; index < argv.length; index += 1) {
    const argument = argv[index];
    if (argument === "--write-manifest") {
      options.writeManifest = true;
    } else if (argument === "--root" && index + 1 < argv.length) {
      index += 1;
      options.root = resolve(argv[index]);
    } else {
      throw new Error(`unknown argument ${argument}`);
    }
  }
  return options;
}

export function main(
  argv,
  { stdout = process.stdout, stderr = process.stderr } = {},
) {
  let options;
  try {
    options = parseArguments(argv);
  } catch (error) {
    stderr.write(`imported_skills_error ${error.message}\n`);
    return 2;
  }
  const root =
    options.root ?? resolve(dirname(fileURLToPath(import.meta.url)), "../..");

  if (options.writeManifest) {
    let manifest;
    try {
      manifest = buildManifest(root);
    } catch (error) {
      stderr.write(`imported_skills_error ${error.message}\n`);
      return 2;
    }
    writeFileSync(
      join(root, ManifestFile),
      `${JSON.stringify(manifest, null, 2)}\n`,
    );
    stdout.write(
      `imported_skills_manifest_written skills=${Object.keys(manifest.skills).length} path=${ManifestFile}\n`,
    );
    return 0;
  }

  const problems = verifyImportedSkills(root);
  for (const problem of problems) {
    stderr.write(
      `imported_skills_problem kind=${problem.kind} ${problem.message}\n`,
    );
  }
  if (problems.length > 0) {
    stderr.write(`imported_skills_failed problems=${problems.length}\n`);
    return 1;
  }
  stdout.write("imported_skills_ok\n");
  return 0;
}

if (
  process.argv[1] !== undefined &&
  resolve(process.argv[1]) === fileURLToPath(import.meta.url)
) {
  process.exitCode = main(process.argv.slice(2));
}
