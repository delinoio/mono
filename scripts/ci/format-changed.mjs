// SPDX-License-Identifier: Apache-2.0
import { execFileSync, spawn } from "node:child_process";
import { lstatSync, readFileSync } from "node:fs";
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
const git = (cwd, ...args) =>
  execFileSync("git", args, {
    cwd,
    encoding: "utf8",
    maxBuffer: 32 * 1024 * 1024,
  });

// Top level of the working tree that contains `start`. `git diff` prints
// repository-root-relative paths while `git ls-files` prints paths relative to
// (and limited to) its working directory, so every selector, the Rstack probe
// and the formatter run from this one root. The invoking directory then never
// changes which files are selected or how a selected path resolves. Only the
// single newline that terminates Git's output is removed: the path itself may
// legitimately end in whitespace (a checkout directory named `checkout `), and
// trimming it would point every selector at a directory that does not exist.
function repositoryRoot(start) {
  return git(start, "rev-parse", "--show-toplevel").replace(/\n$/u, "");
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
// and change what Git records. A gitlink is a directory in the worktree and
// has no blob to format. `git ls-files --stage` reads the index that Git
// resolves from the environment (honoring GIT_INDEX_FILE inside hooks), so it
// is the validation index, works before the first commit and never consults the
// comparison head. Each entry is `<mode> <object> <stage>\t<path>`; the path
// follows the first tab and may itself contain tabs or newlines.
function recordedPaths(cwd, modes) {
  const paths = new Set();
  for (const entry of git(cwd, "ls-files", "--stage", "-z").split("\0")) {
    const tab = entry.indexOf("\t");
    if (tab === -1) continue;
    if (modes.includes(entry.slice(0, tab).split(" ")[0])) {
      paths.add(entry.slice(tab + 1));
    }
  }
  return paths;
}

const recordedSymlinks = (cwd) => recordedPaths(cwd, [SymlinkMode]);

export function allFormatFiles(start) {
  const cwd = repositoryRoot(start);
  const symlinks = recordedSymlinks(cwd);
  const names = git(cwd, "ls-files", "-z")
    .split("\0")
    .filter((name) => {
      if (!isFormattableExtension(name) || symlinks.has(name)) return false;
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
      [rstackEntry(), "fmt", "--stdin-filepath", `./${name}`],
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
function stagedRegularFile(cwd, name) {
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
  const exempt = recordedPaths(cwd, [SymlinkMode, GitlinkMode]);
  return git(
    cwd,
    "diff",
    "--cached",
    "--name-only",
    "--no-renames",
    "--diff-filter=ACMRT",
    "-z",
    "--",
  )
    .split("\0")
    .filter((name) => {
      // Every exemption runs before the worktree is consulted, so none of them
      // can hit the missing-file or non-regular-file failure: an unsupported
      // extension, a path the index records as a symlink or gitlink, and,
      // inside stagedRegularFile, a path excluded by the shared Rstack
      // configuration.
      if (!isFormattableExtension(name) || exempt.has(name)) return false;
      return stagedRegularFile(cwd, name);
    })
    .sort();
}

export function changedFormatFiles(start, base, head, local = false) {
  const cwd = repositoryRoot(start);
  revision(cwd, base);
  revision(cwd, head);
  const names = git(
    cwd,
    "diff",
    "--name-only",
    "--no-renames",
    "-z",
    base,
    head,
    "--",
  ).split("\0");
  if (local) {
    // `git diff <head>` compares the tree with the working tree, so a path whose
    // index differs from HEAD but whose working tree equals HEAD (for example
    // after `git add` and a later revert) is invisible to it. Add the staged
    // delta explicitly so such paths are still selected.
    for (const extra of [
      ["diff", "--name-only", "--no-renames", "-z", head, "--"],
      ["diff", "--cached", "--name-only", "--no-renames", "-z", head, "--"],
    ]) {
      names.push(...git(cwd, ...extra).split("\0"));
    }
    names.push(
      ...git(cwd, "ls-files", "--others", "--exclude-standard", "-z").split(
        "\0",
      ),
    );
  }
  // The checkout may be a PR merge commit. Select paths from the planner range,
  // then check the actual validation tree; never replace it with head contents.
  const present = new Set(
    git(
      cwd,
      "ls-files",
      "--cached",
      "--others",
      "--exclude-standard",
      "-z",
    ).split("\0"),
  );
  // The validation index decides which paths are symlinks, not the comparison
  // head: the checkout may be a merge commit that differs from `head`.
  const symlinks = recordedSymlinks(cwd);
  return [...new Set(names)]
    .filter((name) => {
      if (!name || !isFormattableExtension(name) || !present.has(name))
        return false;
      if (symlinks.has(name)) return false;
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

export async function runFormatter(start, paths, mode = Mode.Check) {
  if (!Object.values(Mode).includes(mode))
    throw new Error("Unknown formatting mode");
  if (!paths.length) return 0;
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
    if (signal) return signalExitCode(signal);
    status = Math.max(status, batchStatus);
  }
  return status;
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
  const status = await runFormatter(cwd, paths, mode);
  console.log(
    JSON.stringify({
      event: "ci_format_result",
      status,
      candidates: paths.length,
    }),
  );
  if (pathspecCommit && status !== 0)
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
