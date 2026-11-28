// SPDX-License-Identifier: Apache-2.0

/** The kind of one line of a line diff. */
export enum DiffLineKind {
  Same = "same",
  Removed = "removed",
  Added = "added",
}

export interface DiffLine {
  kind: DiffLineKind;
  text: string;
}

/**
 * Above this many line pairs the diff does not search for common lines and
 * shows the old text as removed and the new text as added, so a large file
 * cannot block the page.
 */
export const MAX_DIFF_CELLS = 250_000;

function splitLines(text: string): string[] {
  if (text === "") return [];
  return text.replaceAll("\r\n", "\n").replace(/\n$/, "").split("\n");
}

/**
 * A line diff of a tool call `Diff`: the longest common subsequence of the
 * lines, with removed lines before added lines in each changed run. An
 * unset old text (a new file) shows every line as added.
 */
export function diffLines(
  oldText: string | undefined,
  newText: string,
): DiffLine[] {
  const a = splitLines(oldText ?? "");
  const b = splitLines(newText);
  if (a.length * b.length > MAX_DIFF_CELLS) {
    return [
      ...a.map((text) => ({ kind: DiffLineKind.Removed, text })),
      ...b.map((text) => ({ kind: DiffLineKind.Added, text })),
    ];
  }
  // lengths[i][j]: LCS length of a[i:] and b[j:].
  const width = b.length + 1;
  const lengths = new Uint32Array((a.length + 1) * width);
  for (let i = a.length - 1; i >= 0; i--) {
    for (let j = b.length - 1; j >= 0; j--) {
      lengths[i * width + j] =
        a[i] === b[j]
          ? lengths[(i + 1) * width + j + 1]! + 1
          : Math.max(
              lengths[(i + 1) * width + j]!,
              lengths[i * width + j + 1]!,
            );
    }
  }
  const out: DiffLine[] = [];
  let i = 0;
  let j = 0;
  while (i < a.length && j < b.length) {
    if (a[i] === b[j]) {
      out.push({ kind: DiffLineKind.Same, text: a[i]! });
      i++;
      j++;
    } else if (lengths[(i + 1) * width + j]! >= lengths[i * width + j + 1]!) {
      out.push({ kind: DiffLineKind.Removed, text: a[i]! });
      i++;
    } else {
      out.push({ kind: DiffLineKind.Added, text: b[j]! });
      j++;
    }
  }
  for (; i < a.length; i++)
    out.push({ kind: DiffLineKind.Removed, text: a[i]! });
  for (; j < b.length; j++) out.push({ kind: DiffLineKind.Added, text: b[j]! });
  return out;
}
