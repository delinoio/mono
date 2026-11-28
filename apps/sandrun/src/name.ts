// SPDX-License-Identifier: Apache-2.0

export const API_KEY_NAME_LIMIT = 64;
export const AGENT_INTEGRATION_NAME_LIMIT = 64;
export const ORGANIZATION_NAME_LIMIT = 100;

export enum DisplayNameError {
  Length,
  ControlCharacter,
}

/** Checks the length and control characters of a trimmed display name. */
export function validateDisplayName(
  name: string,
  limit: number,
): DisplayNameError | undefined {
  let count = 0;
  for (const _ of name) {
    if (++count > limit) return DisplayNameError.Length;
  }
  if (count === 0) return DisplayNameError.Length;
  if (/\p{Cc}/u.test(name)) return DisplayNameError.ControlCharacter;
}
