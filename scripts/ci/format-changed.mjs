// SPDX-License-Identifier: Apache-2.0
import { isUtf8 } from "node:buffer";
import { execFileSync, spawn } from "node:child_process";
import { existsSync, lstatSync, readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { constants } from "node:os";
import { dirname, extname, resolve } from "node:path";
import { parseArgs } from "node:util";

const require = createRequire(import.meta.url);
const extensions = new Set([
  ".js",
  ".jsx",
  ".mjs",
  ".cjs",
  ".ts",
  ".tsx",
  ".mts",
  ".cts",
  ".css",
  ".json",
  ".jsonc",
  ".json5",
  ".yaml",
  ".yml",
  ".md",
  ".mdx",
]);

// Supported extensions are matched case-insensitively so `notes.MD`,
// `Component.TSX` and `data.JSON` are selected like their lowercase forms.
// Every selector shares this predicate so they cannot disagree on case.
function isFormattableExtension(name) {
  return extensions.has(extname(name).toLowerCase());
}

const Mode = Object.freeze({ Check: "check", Write: "write" });
const GitMaxBuffer = 32 * 1024 * 1024;
const git = (cwd, ...args) =>
  execFileSync("git", args, {
    cwd,
    encoding: "utf8",
    maxBuffer: GitMaxBuffer,
  });

// A path whose name is not valid UTF-8 (for example the Latin-1 name
// `caf\xe9.ts` created on Linux) cannot be addressed by the formatter: Node
// passes arguments to Rstack as UTF-8 and Rstack decodes its own file lists
// lossily. A plain UTF-8 decode of Git's `-z` output turns each invalid byte
// into U+FFFD, a name that does not exist on disk, so such a path used to look
// missing (ENOENT). `NonUtf8PathError` reports it accurately instead.
export class NonUtf8PathError extends Error {
  constructor(displayPath) {
    super(
      `Path name is not valid UTF-8 and cannot be formatted: ${displayPath}. The formatter cannot address a file name that is not valid UTF-8. Rename the file to a valid UTF-8 name ("git mv" for a tracked path) and commit the rename.`,
    );
    this.name = "NonUtf8PathError";
    this.path = displayPath;
  }
}

// The checkout path itself is not valid UTF-8. Node decodes `process.cwd()` and
// Git's output as UTF-8, so the path it holds has U+FFFD in place of each
// invalid byte and names a directory that does not exist; passing it as a child
// process `cwd` fails with the same `ENOENT` as a missing Git executable.
export class NonUtf8RootError extends Error {
  constructor(displayPath) {
    super(
      `The checkout path is not valid UTF-8 and cannot be used as a repository root: ${displayPath}. The formatter resolves every selector, the shared-exclusion probe and the Rstack run against this path, and a directory whose name is not valid UTF-8 cannot be addressed. Move or rename the checkout to a path that is valid UTF-8 and run the command again.`,
    );
    this.name = "NonUtf8RootError";
    this.path = displayPath;
  }
}

// Byte length of the UTF-8 sequence that a lead byte announces (1 for a byte
// that cannot lead a sequence; the caller then rejects it as invalid).
function utf8SequenceLength(lead) {
  if (lead >= 0xf0 && lead <= 0xf4) return 4;
  if (lead >= 0xe0 && lead <= 0xef) return 3;
  if (lead >= 0xc2 && lead <= 0xdf) return 2;
  return 1;
}

// Byte-level path identity. Every path is held as a string, so a name that is
// not valid UTF-8 needs a lossless form. Decoding it with U+FFFD would make it
// equal to the valid name that really contains U+FFFD, and a rename from the
// raw `caf\xe9.ts` to the valid `caf\ufffd.ts` would mark the valid
// replacement as invalid. Instead each byte that is not part of a valid UTF-8
// sequence (always 0x80-0xFF) becomes the lone low surrogate U+DC00 + byte, as
// Python's `surrogateescape` does. A decoded valid UTF-8 string never contains
// a lone surrogate, so the mapping is injective: two names are equal exactly
// when their bytes are equal, in every list that Git prints.
const InvalidByteBase = 0xdc00;
const invalidByteEscape = /[\udc80-\udcff]/gu;

// Whether the decoded name holds a byte that is not valid UTF-8.
const hasInvalidBytes = (name) => /[\udc80-\udcff]/u.test(name);

// Decodes a raw path name. Valid UTF-8 decodes as text (Buffer decoding, unlike
// TextDecoder, keeps a leading U+FEFF) and every other byte is escaped.
function decodePath(raw) {
  if (isUtf8(raw)) return raw.toString("utf8");
  let text = "";
  for (let index = 0; index < raw.length;) {
    const length = Math.min(utf8SequenceLength(raw[index]), raw.length - index);
    const chunk = raw.subarray(index, index + length);
    if (isUtf8(chunk)) {
      text += chunk.toString("utf8");
      index += length;
    } else {
      text += String.fromCharCode(InvalidByteBase + raw[index]);
      index += 1;
    }
  }
  return text;
}

// Display form of a decoded name: valid text as is and every byte that is not
// part of a valid sequence as `\xNN`, so the offending byte is visible.
const displayPath = (name) =>
  name.replace(
    invalidByteEscape,
    (escaped) =>
      `\\x${(escaped.charCodeAt(0) - InvalidByteBase).toString(16).padStart(2, "0")}`,
  );

// The name as Node passes it to a child process or the file system: each
// escaped byte becomes U+FFFD. It is only for the Rstack exclusion probe, which
// matches on the valid directory prefixes and never needs the exact bytes.
const lossyPath = (name) => name.replace(invalidByteEscape, "\ufffd");

// Runs a Git command that prints a NUL-separated path list and returns the
// entries as strings in the byte-level form of `decodePath`. Valid UTF-8
// entries decode exactly as before. Empty entries (the terminator after the
// last path) are dropped.
function gitPaths(cwd, args) {
  const output = execFileSync("git", args, {
    cwd,
    encoding: "buffer",
    maxBuffer: GitMaxBuffer,
  });
  const names = [];
  for (let start = 0; start < output.length;) {
    let end = output.indexOf(0, start);
    if (end === -1) end = output.length;
    const raw = output.subarray(start, end);
    start = end + 1;
    if (raw.length === 0) continue;
    names.push(decodePath(raw));
  }
  return names;
}

// Top level of the working tree that contains `start`. `git diff` prints
// repository-root-relative paths while `git ls-files` prints paths relative to
// (and limited to) its working directory, so every selector, the Rstack probe
// and the formatter run from this one root. The invoking directory then never
// changes which files are selected or how a selected path resolves. Only the
// single newline that terminates Git's output is removed: the path itself may
// legitimately end in whitespace (a checkout directory named `checkout `), and
// trimming it would point every selector at a directory that does not exist.
function repositoryRoot(start) {
  try {
    return git(start, "rev-parse", "--show-toplevel").replace(/\n$/u, "");
  } catch (error) {
    throw nonUtf8RootDiagnosis(start, error) ?? error;
  }
}

// Workaround for a checkout whose own path is not valid UTF-8. Why: Node cannot
// represent such a path, so `start` (usually `process.cwd()`) holds U+FFFD for
// each invalid byte, `execFileSync` fails with `ENOENT` for the missing `cwd`,
// and the error is indistinguishable from a missing Git executable. Scope: it
// only diagnoses the checkout that contains the current directory, found by
// asking Git without a `cwd`; a library caller that passes the lossy path of
// another checkout keeps the original error, as does a missing Git. Removal:
// when the formatter can run from such a root (that needs every child process
// to avoid absolute paths) and this diagnosis is no longer needed.
function nonUtf8RootDiagnosis(start, error) {
  if (error?.code !== "ENOENT" || existsSync(start)) return null;
  let raw;
  try {
    raw = execFileSync("git", ["rev-parse", "--show-toplevel"], {
      encoding: "buffer",
      maxBuffer: GitMaxBuffer,
    });
  } catch {
    return null;
  }
  const root = decodePath(raw).replace(/\n$/u, "");
  return hasInvalidBytes(root) ? new NonUtf8RootError(displayPath(root)) : null;
}

function revision(cwd, value) {
  if (!/^[0-9a-f]{40}$/u.test(value ?? "") || /^0+$/u.test(value)) {
    throw new Error(
      "Formatting requires a nonzero 40-character comparison SHA",
    );
  }
  git(cwd, "cat-file", "-e", `${value}^{commit}`);
  return value;
}

// A tracked path is gone when it is missing or when a parent component was
// replaced by a file (ENOTDIR) or a symlink loop (ELOOP); Git still accepts
// such working trees. Every other lstat error stays fatal.
const GoneCode = Object.freeze({
  Missing: "ENOENT",
  NotDirectory: "ENOTDIR",
  SymlinkLoop: "ELOOP",
});
const goneCodes = new Set(Object.values(GoneCode));

function isRegularFile(cwd, name, skipGone = true) {
  try {
    return lstatSync(resolve(cwd, name)).isFile();
  } catch (error) {
    if (skipGone && goneCodes.has(error.code)) return false;
    throw error;
  }
}

// Git index modes of entries whose worktree form is not a formattable blob: a
// tracked symbolic link and a gitlink (submodule commit pointer).
const SymlinkMode = "120000";
const GitlinkMode = "160000";

// Paths recorded with one of `modes` in the current index. A checkout without
// symlink support (Windows without privileges, `core.symlinks=false`) writes
// a symlink path as a regular file holding the link target, so the worktree
// file type alone cannot identify it; formatting would rewrite the placeholder
// and change what Git records. A gitlink is a commit pointer, not a blob: it
// is normally a directory in the worktree, but the exemption follows the
// index mode even when the worktree entry is a regular file. `git ls-files --stage` reads the index that Git
// resolves from the environment (honoring GIT_INDEX_FILE inside hooks), so it
// is the validation index, works before the first commit and never consults the
// comparison head. Each entry is `<mode> <object> <stage>\t<path>`; the path
// follows the first tab and may itself contain tabs or newlines.
function recordedPaths(cwd, modes) {
  const paths = new Set();
  for (const entry of gitPaths(cwd, ["ls-files", "--stage", "-z"])) {
    const tab = entry.indexOf("\t");
    if (tab === -1) continue;
    if (modes.includes(entry.slice(0, tab).split(" ")[0])) {
      paths.add(entry.slice(tab + 1));
    }
  }
  return paths;
}

// Every selector uses this one helper so their exemptions cannot diverge.
const recordedNonFormattablePaths = (cwd) =>
  recordedPaths(cwd, [SymlinkMode, GitlinkMode]);

export function allFormatFiles(start) {
  const cwd = repositoryRoot(start);
  const exempt = recordedNonFormattablePaths(cwd);
  // A name that is not valid UTF-8 stays a silent skip here: Rstack cannot
  // format such a name, so --all keeps skipping it (see the contract). It is
  // skipped by its bytes, never by looking up its lossy form, which may be the
  // name of a different, valid file.
  const names = gitPaths(cwd, ["ls-files", "-z"]).filter((name) => {
    if (hasInvalidBytes(name)) return false;
    if (!isFormattableExtension(name) || exempt.has(name)) return false;
    return isRegularFile(cwd, name);
  });
  // `git ls-files` prints an unmerged path once per index stage (up to three
  // times after a conflicted merge), so deduplicate to keep the candidate
  // count and the formatter arguments to one entry per path.
  return [...new Set(names)].sort();
}

function rstackEntry() {
  const manifestPath = require.resolve("rstack/package.json");
  const manifest = JSON.parse(readFileSync(manifestPath, "utf8"));
  return resolve(dirname(manifestPath), manifest.bin.rs);
}

// Content that every supported extension formats to an empty file but that an
// excluded path leaves untouched (verified for each entry of `extensions`).
const ExclusionProbe = "\n\n\n";

// Whether the shared Rstack configuration (`rstack.config.ts` ignorePatterns
// and Rstack's own ignore handling) excludes the path from formatting. Rstack
// is the single owner of those exclusions and only evaluates them for paths it
// is given, so ask it through `--stdin-filepath`, which applies the same
// matching to a path that does not exist on disk. A probe that cannot run
// reports "not excluded", which keeps the missing-file failure in force.
function excludedByRstack(cwd, name) {
  try {
    const output = execFileSync(
      process.execPath,
      [rstackEntry(), "fmt", "--stdin-filepath", `./${lossyPath(name)}`],
      {
        cwd,
        input: ExclusionProbe,
        encoding: "utf8",
        stdio: ["pipe", "pipe", "pipe"],
      },
    );
    return output === ExclusionProbe;
  } catch {
    return false;
  }
}

// Human-readable kind of a non-regular lstat result for diagnostics.
function entryKind(stats) {
  if (stats.isDirectory()) return "directory";
  if (stats.isSymbolicLink()) return "symbolic link";
  if (stats.isFIFO()) return "FIFO";
  if (stats.isSocket()) return "socket";
  if (stats.isBlockDevice()) return "block device";
  if (stats.isCharacterDevice()) return "character device";
  return "non-regular file";
}

// Staged deletions are excluded by --diff-filter, so every selected path is a
// staged addition, modification, rename or type change. Such a path that is
// missing from the working tree (removed after `git add`, or outside a sparse
// checkout) or that exists as a non-regular entry (for example a directory
// that replaced the file) has a staged blob that `--write` would never format.
// Fail naming the path instead of silently skipping it, so the commit cannot
// bypass formatting. A path that the shared exclusions skip is never formatted
// even when present, so its absence or type is irrelevant and, like the
// unstaged selectors, it is skipped. Symlinks and gitlinks recorded in the
// index are exempted by the caller before the worktree is consulted.
// A `name` that is not valid UTF-8 has no lossless worktree path. After the
// exclusion probe, which still matches because directory prefixes are valid
// UTF-8, such a path fails with the accurate non-UTF-8 error instead of the
// false missing-path diagnosis.
function stagedRegularFile(cwd, name) {
  if (hasInvalidBytes(name)) {
    if (excludedByRstack(cwd, name)) return false;
    throw new NonUtf8PathError(displayPath(name));
  }
  let stats;
  try {
    stats = lstatSync(resolve(cwd, name));
  } catch (error) {
    if (!goneCodes.has(error.code)) throw error;
    if (excludedByRstack(cwd, name)) return false;
    throw new Error(
      `Staged path is missing from the working tree (${error.code}) and cannot be formatted: ${name}. Restore the file, or unstage it with "git restore --staged -- <path>" (or "git rm --cached -- <path>" to commit its deletion), then commit again.`,
      { cause: error },
    );
  }
  if (stats.isFile()) return true;
  if (excludedByRstack(cwd, name)) return false;
  throw new Error(
    `Staged path exists in the working tree as a ${entryKind(stats)}, not a regular file, and cannot be formatted: ${name}. Restore the regular file, or unstage it with "git restore --staged -- <path>" (or "git rm --cached -- <path>" to commit its deletion), then commit again.`,
  );
}

export function stagedFormatFiles(start) {
  const cwd = repositoryRoot(start);
  const exempt = recordedNonFormattablePaths(cwd);
  return gitPaths(cwd, [
    "diff",
    "--cached",
    "--name-only",
    "--no-renames",
    "--diff-filter=ACMRT",
    "-z",
    "--",
  ])
    .filter((name) => {
      // Every exemption runs before the worktree is consulted, so none of them
      // can hit the missing-file or non-regular-file failure: an unsupported
      // extension, a path the index records as a symlink or gitlink, and,
      // inside stagedRegularFile, a path excluded by the shared Rstack
      // configuration. A name that is not valid UTF-8 reaches the same probe
      // before its own failure.
      if (!isFormattableExtension(name) || exempt.has(name)) return false;
      return stagedRegularFile(cwd, name);
    })
    .sort();
}

// What an explicit comparison does with a present formattable path whose name
// is not valid UTF-8 and that no exemption skipped. `Fail` is the formatter
// behavior: after the shared-exclusion probe it throws `NonUtf8PathError`.
// `Defer` selects the path without probing, for the CI planner: the planner
// runs without installed dependencies, so it cannot evaluate the Rstack
// exclusions, and the probe's "cannot run" fallback would wrongly reject an
// excluded name. The planner only counts the selected names (it never passes
// them on as path arguments); the `quality` job's formatter runs with Rstack
// installed and gives the verdict.
export const NonUtf8Policy = Object.freeze({
  Fail: "fail",
  Defer: "defer",
});

export function changedFormatFiles(
  start,
  base,
  head,
  local = false,
  nonUtf8 = NonUtf8Policy.Fail,
) {
  const cwd = repositoryRoot(start);
  revision(cwd, base);
  revision(cwd, head);
  const names = gitPaths(cwd, [
    "diff",
    "--name-only",
    "--no-renames",
    "-z",
    base,
    head,
    "--",
  ]);
  if (local) {
    // `git diff <head>` compares the tree with the working tree, so a path whose
    // index differs from HEAD but whose working tree equals HEAD (for example
    // after `git add` and a later revert) is invisible to it. Add the staged
    // delta explicitly so such paths are still selected.
    for (const extra of [
      ["diff", "--name-only", "--no-renames", "-z", head, "--"],
      ["diff", "--cached", "--name-only", "--no-renames", "-z", head, "--"],
    ]) {
      names.push(...gitPaths(cwd, extra));
    }
    names.push(
      ...gitPaths(cwd, ["ls-files", "--others", "--exclude-standard", "-z"]),
    );
  }
  // The checkout may be a PR merge commit. Select paths from the planner range,
  // then check the actual validation tree; never replace it with head contents.
  // The index alone defines that tree in explicit mode: an untracked file is
  // working-tree state, so one that shares its name with a path the range
  // deleted (or never added to the index) must not make the path a candidate.
  // Only the local selection reads untracked files.
  const present = new Set(
    gitPaths(
      cwd,
      local
        ? ["ls-files", "--cached", "--others", "--exclude-standard", "-z"]
        : ["ls-files", "--cached", "-z"],
    ),
  );
  // The validation index decides which paths are symlinks or gitlinks, not the
  // comparison head: the checkout may be a merge commit that differs from
  // `head`. Both have no blob to format, so they are exempted before the
  // non-UTF-8 check below, as `stagedFormatFiles` does.
  const exempt = recordedNonFormattablePaths(cwd);
  return [...new Set(names)]
    .filter((name) => {
      if (!name || !isFormattableExtension(name) || !present.has(name))
        return false;
      if (exempt.has(name)) return false;
      // The explicit comparison decides what CI validates, so a present path
      // whose name is not valid UTF-8 fails accurately after the exclusion
      // probe. Local selection keeps the silent skip. Names compare by their
      // bytes, so a path deleted in the range is not in the explicit `present`
      // and was skipped above, even when a valid name with the same lossy form
      // exists.
      // The `Defer` policy selects the name before any probe (see
      // `NonUtf8Policy`).
      if (hasInvalidBytes(name)) {
        if (local) return false;
        if (nonUtf8 === NonUtf8Policy.Defer) return true;
        if (excludedByRstack(cwd, name)) return false;
        throw new NonUtf8PathError(displayPath(name));
      }
      return isRegularFile(cwd, name, local);
    })
    .sort();
}

// Signals that stop the formatter. They are forwarded to the Rstack child so an
// external `kill`, `timeout` or supervisor cannot leave it running (and, in
// write mode, rewriting files) after this process is gone. A terminal Ctrl-C
// also reaches the child through the foreground process group, so it receives
// SIGINT twice; real Rstack tolerates that, and skipping the forward on a TTY
// would orphan the child when only this process is signalled with `kill`.
const ForwardedSignal = Object.freeze({
  Hangup: "SIGHUP",
  Interrupt: "SIGINT",
  Terminate: "SIGTERM",
});
const signalExitCode = (signal) => 128 + constants.signals[signal];

// Runs one Rstack batch. Resolves after the child has exited with its status
// and the first forwarded signal received meanwhile (null when none). The
// handlers are removed on every exit path so batches never stack listeners.
function runBatch(cwd, args) {
  return new Promise((resolvePromise, reject) => {
    const child = spawn(process.execPath, args, {
      cwd,
      stdio: "inherit",
      shell: false,
    });
    let received = null;
    const handlers = Object.values(ForwardedSignal).map((signal) => {
      const handler = () => {
        received ??= signal;
        console.error(JSON.stringify({ event: "ci_format_signal", signal }));
        child.kill(signal);
      };
      process.on(signal, handler);
      return [signal, handler];
    });
    const cleanup = () => {
      for (const [signal, handler] of handlers) process.off(signal, handler);
    };
    child.once("error", (error) => {
      cleanup();
      reject(error);
    });
    child.once("close", (code) => {
      cleanup();
      resolvePromise({ status: code ?? 2, signal: received });
    });
  });
}

// Runs every Rstack batch and returns `{ status, signal }`. `signal` is the
// first forwarded signal (null when none); a stopped run reports the shell
// convention `128 + signal` as `status` and starts no further batch. Callers
// that must tell a stopped run from a failed check read `signal`, because a
// signal exit code is indistinguishable from a check status by value alone.
async function runFormatterBatches(start, paths, mode) {
  if (!Object.values(Mode).includes(mode))
    throw new Error("Unknown formatting mode");
  if (!paths.length) return { status: 0, signal: null };
  // Selected paths are repository-root-relative, so Rstack must run there too.
  const cwd = repositoryRoot(start);
  const entry = rstackEntry();
  let status = 0;
  // Bound argv size and prefix paths so leading dashes and negation characters
  // cannot become CLI flags or exclusion patterns. Rstack resolves files first.
  for (let index = 0; index < paths.length; index += 50) {
    const { status: batchStatus, signal } = await runBatch(cwd, [
      entry,
      "fmt",
      `--${mode}`,
      "--no-cache",
      "--no-error-on-unmatched-pattern",
      ...paths.slice(index, index + 50).map((name) => `./${name}`),
    ]);
    // A stopped run must not start another batch; report the shell convention
    // for the received signal instead of the child's own status.
    if (signal) return { status: signalExitCode(signal), signal };
    status = Math.max(status, batchStatus);
  }
  return { status, signal: null };
}

export async function runFormatter(start, paths, mode = Mode.Check) {
  return (await runFormatterBatches(start, paths, mode)).status;
}

// CI is enabled when the variable is set to anything except an empty value or
// an explicit, case-insensitive "false". Other values, including "0", stay
// enabled so ambiguous CI environments keep the stricter comparison rules.
export function ciEnabled(env = process.env) {
  const value = env.CI?.trim().toLowerCase();
  return value !== undefined && value !== "" && value !== "false";
}

// Git runs the pre-commit hook of a pathspec commit (`git commit <path>`,
// `--only`, `--amend <path>`) against a throwaway `next-index-<pid>.lock`
// index. Staging a fix there does not reach the real index: Git writes the real
// index from its own pre-hook snapshot, leaving HEAD formatted and the index
// stale (`MM`). The real index stays locked while the hook runs, so the hook
// cannot repair it. The other commit forms use `index` (plain commit, linked
// worktree) or `index.lock` (`-a`, `-i`), which Git keeps.
// Remove this guard only when Git applies hook changes of a pathspec commit to
// the real index.
export function isThrowawayIndex(env) {
  const path = env.GIT_INDEX_FILE;
  if (!path) return false;
  const name = path.split(/[\\/]/u).pop();
  return /^next-index-.+\.lock$/u.test(name);
}

export async function main(env = process.env) {
  const { values } = parseArgs({
    options: {
      write: { type: "boolean" },
      all: { type: "boolean" },
      staged: { type: "boolean" },
      base: { type: "string" },
      head: { type: "string" },
    },
  });
  const cwd = repositoryRoot(process.cwd());
  const ci = ciEnabled(env);
  const explicit =
    values.base !== undefined ||
    values.head !== undefined ||
    env.TURBO_SCM_BASE !== undefined ||
    env.TURBO_SCM_HEAD !== undefined;
  if (values.staged && (ci || values.all || explicit))
    throw new Error(
      "Staged formatting requires local execution without a comparison or --all",
    );
  if (ci && !explicit)
    throw new Error("CI formatting requires the planner comparison");
  const head =
    values.staged || values.all
      ? null
      : explicit
        ? (values.head ?? env.TURBO_SCM_HEAD)
        : git(cwd, "rev-parse", "HEAD").trim();
  const base =
    values.staged || values.all
      ? null
      : explicit
        ? (values.base ?? env.TURBO_SCM_BASE)
        : git(cwd, "merge-base", "origin/main", head).trim();
  if (values.all && ci) throw new Error("CI formatting must use changed files");
  // `--all` selects every tracked file and never reads the comparison, so
  // honoring it would silently drop the requested range and skip revision
  // validation. Reject the contradiction like `--staged` does.
  if (values.all && explicit)
    throw new Error(
      "All-file formatting requires local execution without a comparison",
    );
  const paths = values.staged
    ? stagedFormatFiles(cwd)
    : values.all
      ? allFormatFiles(cwd)
      : changedFormatFiles(cwd, base, head, !explicit);
  const pathspecCommit = Boolean(values.staged) && isThrowawayIndex(env);
  const mode = values.write && !pathspecCommit ? Mode.Write : Mode.Check;
  console.log(
    JSON.stringify({
      event: "ci_format",
      base,
      head,
      validationRevision: values.staged
        ? null
        : git(cwd, "rev-parse", "HEAD").trim(),
      staged: values.staged ?? false,
      mode,
      candidates: paths.length,
      local: !explicit,
    }),
  );
  const { status, signal } = await runFormatterBatches(cwd, paths, mode);
  console.log(
    JSON.stringify({
      event: "ci_format_result",
      status,
      candidates: paths.length,
    }),
  );
  // A run stopped by a signal exits 128+signal and was not a formatting verdict,
  // so only a non-signal failure gets the pathspec-commit guidance. The signal
  // is checked directly: `status > 128` would also match a child's own status.
  if (pathspecCommit && !signal && status !== 0)
    // Check mode exits 1 for both unformatted files and parser failures.
    throw new Error(
      "git commit <path> cannot be formatted through the pre-commit hook: " +
        "formatting is required or a staged file cannot be parsed. " +
        "Nothing was changed. Fix the reported files, run `git add` on them " +
        "and run a plain `git commit` without a path.",
    );
  return status;
}

// import.meta.main needs Node 24.2+ (Rstack already requires 24.3+). Unlike
// comparing process.argv[1] with the module URL, it also holds when the script
// path goes through a symlink.
if (import.meta.main) {
  try {
    process.exitCode = await main();
  } catch (error) {
    console.error(
      JSON.stringify({ event: "ci_format_error", message: error.message }),
    );
    process.exitCode = 2;
  }
}
