// SPDX-License-Identifier: Apache-2.0

// `instanceof` compares constructor identity, so it misses a Uint8Array or an
// ArrayBuffer from another realm (a `node:vm` context, an iframe or a worker
// boundary): such a value would fall through to a generic branch that fails or
// treats it as another kind of value (the write source of `writeFile` as an
// iterable, a data channel message as non-binary data). These checks read the internal slots with
// the intrinsic accessors of this realm instead, which accept an object of
// any realm and reject every other object, including one that only fakes
// `Symbol.toStringTag`. They use no Node.js module, so the SDK stays
// runtime-neutral.
const typedArrayTagOf = Object.getOwnPropertyDescriptor(
  Object.getPrototypeOf(Uint8Array.prototype),
  Symbol.toStringTag,
)?.get as (this: unknown) => string | undefined;
const arrayBufferByteLengthOf = Object.getOwnPropertyDescriptor(
  ArrayBuffer.prototype,
  "byteLength",
)?.get as (this: unknown) => number;

/** Reports whether `value` is a Uint8Array of any realm (a Buffer included). */
export function isUint8Array(value: unknown): value is Uint8Array {
  return (
    typeof value === "object" &&
    value !== null &&
    typedArrayTagOf.call(value) === "Uint8Array"
  );
}

/** Reports whether `value` is an ArrayBuffer of any realm. */
export function isArrayBuffer(value: unknown): value is ArrayBuffer {
  if (typeof value !== "object" || value === null) return false;
  try {
    // Throws a TypeError for anything that has no ArrayBuffer slot, and a
    // SharedArrayBuffer has its own accessor, so it also throws here.
    arrayBufferByteLengthOf.call(value);
    return true;
  } catch {
    return false;
  }
}
