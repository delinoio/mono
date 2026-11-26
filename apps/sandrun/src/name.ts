// SPDX-License-Identifier: Apache-2.0

export const API_KEY_NAME_LIMIT = 64;
export const AGENT_INTEGRATION_NAME_LIMIT = 64;
export const ORGANIZATION_NAME_LIMIT = 100;

/** Checks the length of a trimmed display name in Unicode code points. */
export function isNameLengthValid(name: string, limit: number): boolean {
  let count = 0;
  for (const _ of name) {
    if (++count > limit) return false;
  }
  return count > 0;
}
