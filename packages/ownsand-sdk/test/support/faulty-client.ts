// SPDX-License-Identifier: Apache-2.0
import type { UniversalClientFn } from "@connectrpc/connect/protocol";

import type { Clock } from "../../src/clock.ts";

/** Response metadata that tells the test HTTP client to cut the connection. */
export const DISCONNECT_HEADER = "x-fake-disconnect";
/** Response header `<pieces>:<intervalMs>` that delivers large messages slowly. */
export const DRIP_HEADER = "x-fake-drip";

const decoder = new TextDecoder();

/** The error that a cut connection surfaces to the transport. */
export function connectionReset(): Error {
  return new Error("fake connection reset");
}

/**
 * Wraps an HTTP client and turns the fake server's disconnect marker into a
 * real transport failure: the unary response or the stream body fails without
 * a valid terminal status, as when a connection closes during a control
 * plane restart. It can also deliver large stream messages in slow pieces.
 */
export function faultyClient(
  inner: UniversalClientFn,
  clock: Clock,
): UniversalClientFn {
  return async (request) => {
    const response = await inner(request);
    if (response.header.has(DISCONNECT_HEADER)) {
      throw connectionReset();
    }
    const drip = response.header.get(DRIP_HEADER);
    const [pieces, interval] = drip ? drip.split(":").map(Number) : [1, 0];
    return {
      ...response,
      body: transform(response.body, pieces!, interval!, clock, request.signal),
    };
  };
}

async function* transform(
  body: AsyncIterable<Uint8Array>,
  pieces: number,
  intervalMs: number,
  clock: Clock,
  signal: AbortSignal | undefined,
): AsyncIterable<Uint8Array> {
  let buf = new Uint8Array(0);
  for await (const chunk of body) {
    const merged = new Uint8Array(buf.byteLength + chunk.byteLength);
    merged.set(buf);
    merged.set(chunk, buf.byteLength);
    buf = merged;
    for (;;) {
      if (buf.byteLength < 5) {
        break;
      }
      const len = new DataView(buf.buffer, buf.byteOffset + 1, 4).getUint32(0);
      if (buf.byteLength < 5 + len) {
        break;
      }
      const envelope = buf.subarray(0, 5 + len);
      buf = buf.subarray(5 + len);
      const endStream = (envelope[0]! & 0x02) !== 0;
      if (
        endStream &&
        decoder.decode(envelope.subarray(5)).includes(DISCONNECT_HEADER)
      ) {
        throw connectionReset();
      }
      if (!endStream && pieces > 1 && len > 1024) {
        const size = Math.ceil(envelope.byteLength / pieces);
        for (let o = 0; o < envelope.byteLength; o += size) {
          if (o > 0) {
            await clock.sleep(intervalMs, signal);
          }
          yield envelope.slice(o, o + size);
        }
      } else {
        yield envelope.slice();
      }
    }
  }
  if (buf.byteLength > 0) {
    yield buf;
  }
}
