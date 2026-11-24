// SPDX-License-Identifier: Apache-2.0

const MAX_TIMESTAMP_MS = 2 ** 48 - 1;

/**
 * UUID v7 generation for the identifiers that the client generates
 * (`exec_id`, `read_id` and `write_id`). The layout follows RFC 9562: a
 * 48-bit Unix millisecond timestamp, the version, a 12-bit counter in
 * `rand_a` that starts from a random value each millisecond, the variant and
 * 62 random bits. Values are strictly increasing within one generator. It
 * uses the Web Crypto API, so it runs in Node.js and in browsers.
 */
export class UuidV7Generator {
  #lastMs = -1;
  #counter = 0;
  readonly #nowMs: () => number;

  constructor(nowMs: () => number = Date.now) {
    this.#nowMs = nowMs;
  }

  /**
   * Returns the next UUID v7. Throws RangeError if the floored clock reading
   * or a counter carry exceeds the 48-bit timestamp limit. The generator
   * keeps its last successful timestamp and counter on failure.
   */
  next(): string {
    let ms = Math.floor(this.#nowMs());
    let counter: number | undefined;
    if (ms <= this.#lastMs) {
      // Same millisecond, or a wall clock that went back: keep the last
      // timestamp and advance the counter so values stay increasing.
      ms = this.#lastMs;
      if (this.#counter === 0xfff) {
        ms++;
      } else {
        counter = this.#counter + 1;
      }
    }
    if (ms > MAX_TIMESTAMP_MS) {
      throw new RangeError("UUID v7 timestamp exceeds the 48-bit limit");
    }
    counter ??= randomCounter();

    const bytes = new Uint8Array(16);
    globalThis.crypto.getRandomValues(bytes);
    let rest = ms;
    for (let i = 5; i >= 0; i--) {
      bytes[i] = rest % 256;
      rest = Math.floor(rest / 256);
    }
    bytes[6] = 0x70 | ((counter >> 8) & 0x0f);
    bytes[7] = counter & 0xff;
    bytes[8] = 0x80 | (bytes[8]! & 0x3f);
    const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join(
      "",
    );
    const uuid = `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
    this.#lastMs = ms;
    this.#counter = counter;
    return uuid;
  }
}

/** A random counter start in the lower half, so that it has room to grow. */
function randomCounter(): number {
  const bytes = new Uint8Array(2);
  globalThis.crypto.getRandomValues(bytes);
  return ((bytes[0]! << 8) | bytes[1]!) & 0x7ff;
}

const UUID_V7_PATTERN =
  /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

/** Reports whether value is a UUID v7 in canonical lowercase text form. */
export function isUuidV7(value: string): boolean {
  return UUID_V7_PATTERN.test(value);
}
