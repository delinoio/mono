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
 * Finds the first occurrence of a marker in stdout, even when the marker is
 * split across chunks.
 */
export class MarkerOffsetFinder {
  private readonly marker: Uint8Array;
  private bytes = new Uint8Array(0);
  private firstMarkerEnd: number | undefined;

  constructor(marker: string) {
    this.marker = new TextEncoder().encode(marker);
    if (this.marker.length === 0) {
      throw new RangeError("marker must not be empty");
    }
  }

  /**
   * Returns the byte offset directly after the first marker in all chunks
   * pushed so far, or undefined until a match is found.
   */
  push(chunk: Uint8Array): number | undefined {
    if (this.firstMarkerEnd !== undefined) {
      return this.firstMarkerEnd;
    }

    // Earlier pushes found no match. Start at the earliest byte where a
    // marker could start and still end in the new chunk.
    const searchFrom = Math.max(0, this.bytes.length - this.marker.length + 1);
    const next = new Uint8Array(this.bytes.length + chunk.length);
    next.set(this.bytes, 0);
    next.set(chunk, this.bytes.length);
    this.bytes = next;
    const index = indexOf(this.bytes, this.marker, searchFrom);
    if (index >= 0) {
      this.firstMarkerEnd = index + this.marker.length;
    }
    return this.firstMarkerEnd;
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
