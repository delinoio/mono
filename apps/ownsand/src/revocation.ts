// SPDX-License-Identifier: Apache-2.0

/** Find prefixes shared by two or more distinct, revocable rows. */
export function repeatedPrefixes(prefixes: readonly string[]): Set<string> {
  const seen = new Set<string>();
  const repeated = new Set<string>();
  for (const prefix of prefixes) {
    if (seen.has(prefix)) repeated.add(prefix);
    seen.add(prefix);
  }
  return repeated;
}
