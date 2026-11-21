// SPDX-License-Identifier: Apache-2.0
import { create } from "@bufbuild/protobuf";
import { describe, expect, test } from "@rstest/core";
import {
  encodeFrame,
  encodeMessages,
  FrameCodecError,
  FrameDecoder,
  MAX_DATA_CHANNEL_MESSAGE_SIZE,
  MAX_FRAME_SIZE,
  payloadKind,
} from "./codec.ts";
import {
  DataFrameKind,
  DataFrameSchema,
} from "./gen/delino/ownsand/v1/data_frame_pb.ts";

function chunkFrame(streamId: bigint, size: number) {
  return create(DataFrameSchema, {
    streamId,
    sandboxId: "0192f4b6-7c3a-7def-8abc-0123456789ab",
    kind: DataFrameKind.FILE_READ_EVENT,
    payload: {
      case: "fileReadEvent",
      value: {
        event: {
          case: "chunk",
          value: { offset: 0n, data: new Uint8Array(size).fill(7) },
        },
      },
    },
  });
}

describe("data channel codec", () => {
  test("splits frames into messages of at most 16 KiB and reassembles them", () => {
    const frame = chunkFrame(1n, 1 << 20);
    const messages = encodeMessages(frame);
    expect(messages.length).toBeGreaterThan(60);
    for (const m of messages) {
      expect(m.length).toBeLessThanOrEqual(MAX_DATA_CHANNEL_MESSAGE_SIZE);
    }
    const decoder = new FrameDecoder();
    const frames = messages.flatMap((m) => decoder.push(m));
    expect(frames).toHaveLength(1);
    expect(frames[0]!.streamId).toBe(1n);
    expect(payloadKind(frames[0]!)).toBe(DataFrameKind.FILE_READ_EVENT);
  });

  test("decodes several frames in one message and across random splits", () => {
    const stream = new Uint8Array([
      ...encodeFrame(chunkFrame(1n, 10)),
      ...encodeFrame(chunkFrame(2n, 70_000)),
      ...encodeFrame(chunkFrame(3n, 0)),
    ]);
    for (let round = 0; round < 20; round++) {
      const decoder = new FrameDecoder();
      const ids: bigint[] = [];
      let at = 0;
      while (at < stream.length) {
        const n = 1 + Math.floor(Math.random() * 40_000);
        for (const f of decoder.push(stream.subarray(at, at + n))) {
          ids.push(f.streamId);
        }
        at += n;
      }
      expect(ids).toEqual([1n, 2n, 3n]);
    }
  });

  test("rejects an oversized declared length before buffering and stays failed", () => {
    const decoder = new FrameDecoder();
    // Varint of MAX_FRAME_SIZE + 1.
    let n = MAX_FRAME_SIZE + 1;
    const prefix: number[] = [];
    while (n >= 0x80) {
      prefix.push((n & 0x7f) | 0x80);
      n >>>= 7;
    }
    prefix.push(n);
    expect(() => decoder.push(Uint8Array.from(prefix))).toThrow(
      FrameCodecError,
    );
    expect(() => decoder.push(encodeFrame(chunkFrame(1n, 1)))).toThrow(
      FrameCodecError,
    );
  });

  test("rejects a length prefix longer than 10 bytes", () => {
    const decoder = new FrameDecoder();
    expect(() => decoder.push(new Uint8Array(11).fill(0x80))).toThrow(
      FrameCodecError,
    );
  });

  test("rejects a body that is not a DataFrame", () => {
    const decoder = new FrameDecoder();
    expect(() => decoder.push(Uint8Array.from([2, 0xff, 0xff]))).toThrow(
      FrameCodecError,
    );
  });
});
