// SPDX-License-Identifier: Apache-2.0
import { randomBytes } from "node:crypto";

/**
 * UUID v7 generation for the caller-generated identifiers (`exec_id`,
 * `read_id`, `write_id`, `idempotency_key` and `authorization_id`). The
 * layout follows RFC 9562 and matches the Go `uuidv7` package: a 48-bit
 * Unix millisecond timestamp, the version, a 12-bit counter in `rand_a` that
 * starts from a random value each millisecond, the variant and 62 random
 * bits. Values are strictly increasing within one generator.
 */
export class UuidV7Generator {
  #lastMs = -1;
  #counter = 0;
  readonly #nowMs: () => number;

  constructor(nowMs: () => number = Date.now) {
    this.#nowMs = nowMs;
  }

  next(): string {
    let ms = Math.floor(this.#nowMs());
    if (ms <= this.#lastMs) {
      // Same millisecond, or a wall clock that went back: keep the last
      // timestamp and advance the counter so values stay increasing.
      ms = this.#lastMs;
      this.#counter++;
      if (this.#counter > 0xfff) {
        ms++;
        this.#counter = randomBytes(2).readUInt16BE(0) & 0x7ff;
      }
    } else {
      // Start the counter in the lower half so that it has room to grow.
      this.#counter = randomBytes(2).readUInt16BE(0) & 0x7ff;
    }
    this.#lastMs = ms;
    const bytes = randomBytes(16);
    bytes.writeUIntBE(ms, 0, 6);
    bytes[6] = 0x70 | ((this.#counter >> 8) & 0x0f);
    bytes[7] = this.#counter & 0xff;
    bytes[8] = 0x80 | (bytes[8]! & 0x3f);
    return formatUuid(bytes);
  }
}

const UUID_V7_PATTERN =
  /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

/** Reports whether value is a UUID v7 in canonical lowercase text form. */
export function isUuidV7(value: string): boolean {
  return UUID_V7_PATTERN.test(value);
}

const UUID_V7_ANY_CASE_PATTERN =
  /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;

/**
 * Returns the canonical lowercase text form of a UUID v7 that is spelled in
 * either hex case, and the value unchanged when it is not a UUID v7. The
 * control plane and the agent spell every UUID in lowercase, so a caller
 * named identifier must be lowercased before it reaches a request, a relay
 * request digest or a data channel frame.
 */
export function canonicalizeUuidV7(value: string): string {
  return UUID_V7_ANY_CASE_PATTERN.test(value) ? value.toLowerCase() : value;
}

/** Returns the Unix millisecond timestamp of a UUID v7. */
export function uuidV7TimestampMs(value: string): number {
  if (!isUuidV7(value)) {
    throw new TypeError("not a canonical UUID v7");
  }
  return Number.parseInt(value.slice(0, 8) + value.slice(9, 13), 16);
}

function formatUuid(bytes: Uint8Array): string {
  const hex = Buffer.from(bytes).toString("hex");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}
