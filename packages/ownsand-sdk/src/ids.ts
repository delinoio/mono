// SPDX-License-Identifier: Apache-2.0

/** Age at which a new operation ID is expired on the agent. */
export const OPERATION_ID_MAX_AGE_MS = 15 * 60 * 1000;

/** Returns a new UUID v7 in canonical text form for the given Unix milliseconds. */
export function newUuidV7(unixMs: number): string {
  const b = new Uint8Array(16);
  crypto.getRandomValues(b);
  let ms = Math.max(0, Math.floor(unixMs));
  for (let i = 5; i >= 0; i--) {
    b[i] = ms % 256;
    ms = Math.floor(ms / 256);
  }
  b[6] = (b[6]! & 0x0f) | 0x70; // version 7
  b[8] = (b[8]! & 0x3f) | 0x80; // RFC 9562 variant
  const hex = Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

const UUID_V7 =
  /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;

/** Reports whether `s` is a canonical UUID v7. */
export function isUuidV7(s: string): boolean {
  return UUID_V7.test(s);
}

/** Returns the Unix millisecond timestamp of a UUID v7. */
export function uuidV7Time(id: string): number {
  const hex = id.replaceAll("-", "").slice(0, 12);
  return Number.parseInt(hex, 16);
}

/** Returns 32 random bytes for the binding salt of one operation. */
export function newBindingSalt(): Uint8Array {
  const salt = new Uint8Array(32);
  crypto.getRandomValues(salt);
  return salt;
}
