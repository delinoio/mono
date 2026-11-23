// SPDX-License-Identifier: Apache-2.0
//
// Marker search of the end-to-end test (src/e2e.test.ts). A stdout event is a
// buffered read of the process output, so one event can hold several lines
// and a line can span two events. The search therefore runs over the
// concatenated bytes of the events, and the result is a byte offset (the unit
// of the exec attach offsets), not a string length. It lives apart from the
// test file so that a unit test can cover it without a control plane (the
// end-to-end test is skipped unless the OwnSand end-to-end suite starts it).

/**
 * Accumulates stdout chunks and finds the first occurrence of a marker, even
 * when the marker is split across chunks.
 */
export class MarkerOffsetFinder {
  private readonly marker: Uint8Array;
  private bytes = new Uint8Array(0);

  constructor(marker: string) {
    this.marker = new TextEncoder().encode(marker);
    if (this.marker.length === 0) {
      throw new RangeError("marker must not be empty");
    }
  }

  /**
   * Appends a chunk. Returns the byte offset directly after the first marker
   * in all bytes pushed so far, or undefined when the marker has not been
   * seen yet.
   */
  push(chunk: Uint8Array): number | undefined {
    // The marker cannot end before the previous bytes plus this chunk, so a
    // search that starts one marker length before the new chunk finds every
    // new match and, because earlier pushes found none, the first one.
    const searchFrom = Math.max(0, this.bytes.length - this.marker.length + 1);
    const next = new Uint8Array(this.bytes.length + chunk.length);
    next.set(this.bytes, 0);
    next.set(chunk, this.bytes.length);
    this.bytes = next;
    const index = indexOf(this.bytes, this.marker, searchFrom);
    return index < 0 ? undefined : index + this.marker.length;
  }
}

function indexOf(
  haystack: Uint8Array,
  needle: Uint8Array,
  from: number,
): number {
  for (let i = from; i + needle.length <= haystack.length; i++) {
    let match = true;
    for (let j = 0; j < needle.length; j++) {
      if (haystack[i + j] !== needle[j]) {
        match = false;
        break;
      }
    }
    if (match) {
      return i;
    }
  }
  return -1;
}
