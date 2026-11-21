// SPDX-License-Identifier: Apache-2.0
import { fromBinary, toBinary } from "@bufbuild/protobuf";
import {
  type DataFrame,
  DataFrameKind,
  DataFrameSchema,
} from "./gen/delino/ownsand/v1/data_frame_pb.ts";

/**
 * The data channel codec of the data path framing section of
 * docs/protos-ownsand-api-contract.md, equal to the Go `datapath` package:
 * each frame is a varint length followed by the encoded `DataFrame`, and the
 * byte stream is split into data channel messages of at most 16 KiB.
 */

/** Largest encoded DataFrame: 1 MiB plus 64 KiB. */
export const MAX_FRAME_SIZE = (1 << 20) + (64 << 10);
/** Largest data channel message. */
export const MAX_DATA_CHANNEL_MESSAGE_SIZE = 16 << 10;
/** Largest file chunk. */
export const MAX_FILE_CHUNK_SIZE = 1 << 20;

const MAX_VARINT_LENGTH = 10;

/** The payload case name of each DataFrameKind (kind name in camelCase). */
const KIND_BY_CASE: Readonly<Record<string, DataFrameKind>> = {
  relayStreamOpen: DataFrameKind.RELAY_STREAM_OPEN,
  relayStreamClose: DataFrameKind.RELAY_STREAM_CLOSE,
  relayWindowUpdate: DataFrameKind.RELAY_WINDOW_UPDATE,
  relayKeepalive: DataFrameKind.RELAY_KEEPALIVE,
  supportedFrameKinds: DataFrameKind.SUPPORTED_FRAME_KINDS,
  sessionEnd: DataFrameKind.SESSION_END,
  requestAccepted: DataFrameKind.REQUEST_ACCEPTED,
  execRequest: DataFrameKind.EXEC_REQUEST,
  execAttachRequest: DataFrameKind.EXEC_ATTACH_REQUEST,
  execEvent: DataFrameKind.EXEC_EVENT,
  fileReadRequest: DataFrameKind.FILE_READ_REQUEST,
  fileReadEvent: DataFrameKind.FILE_READ_EVENT,
  fileWriteChunk: DataFrameKind.FILE_WRITE_CHUNK,
  fileWriteResult: DataFrameKind.FILE_WRITE_RESULT,
  error: DataFrameKind.ERROR,
};

/** Returns the kind that names the frame's payload, or UNSPECIFIED. */
export function payloadKind(frame: DataFrame): DataFrameKind {
  const c = frame.payload.case;
  return c === undefined
    ? DataFrameKind.UNSPECIFIED
    : (KIND_BY_CASE[c] ?? DataFrameKind.UNSPECIFIED);
}

/** Error of the data channel codec. The receiver closes the channel. */
export class FrameCodecError extends Error {
  override name = "FrameCodecError";
}

/** The stream_id of connection-level frames. */
export const CONNECTION_STREAM_ID = 0n;

/** A frame that breaks a framing rule of `validateFrame`. */
export class FrameValidationError extends Error {
  override name = "FrameValidationError";
}

/** A frame that is malformed: its kind and payload disagree or a stream_id rule breaks. */
export class MalformedFrameError extends FrameValidationError {
  override name = "MalformedFrameError";
}

/**
 * A frame whose kind this schema version does not know (a newer schema). On
 * stream_id 0 a receiver ignores it; on any other stream it ends the stream.
 */
export class UnknownKindError extends FrameValidationError {
  override name = "UnknownKindError";
  readonly kind: number;
  readonly streamId: bigint;

  constructor(kind: number, streamId: bigint) {
    super(`unknown frame kind ${kind} on stream ${streamId}`);
    this.kind = kind;
    this.streamId = streamId;
  }
}

/** Reports whether the kind is a DataFrameKind of this schema version other than UNSPECIFIED. */
export function isKnownKind(kind: number): boolean {
  return kind >= DataFrameKind.RELAY_STREAM_OPEN && kind <= DataFrameKind.ERROR;
}

/** Reports whether the kind is sent only on stream_id 0 of a relay connection. */
export function isConnectionKind(kind: number): boolean {
  return (
    kind === DataFrameKind.RELAY_KEEPALIVE ||
    kind === DataFrameKind.SUPPORTED_FRAME_KINDS
  );
}

/**
 * Checks the framing rules that do not depend on the transport, equal to the
 * Go `datapath.ValidateFrame`: the kind is set, known and equal to the kind
 * of the payload, connection-level kinds use stream_id 0 with an empty
 * sandbox_id, and other kinds use a stream_id other than 0. It returns an
 * `UnknownKindError` for a kind of a newer schema, a `MalformedFrameError`
 * for any other violation, and `undefined` for a valid frame.
 */
export function validateFrame(
  frame: DataFrame,
): FrameValidationError | undefined {
  const kind: number = frame.kind;
  if (kind === DataFrameKind.UNSPECIFIED) {
    return new MalformedFrameError("frame kind is unspecified");
  }
  if (!isKnownKind(kind)) {
    return new UnknownKindError(kind, frame.streamId);
  }
  const got = payloadKind(frame);
  if (got !== kind) {
    return new MalformedFrameError(
      `frame kind ${DataFrameKind[kind]} carries payload ${DataFrameKind[got]}`,
    );
  }
  if (isConnectionKind(kind)) {
    if (frame.streamId !== CONNECTION_STREAM_ID || frame.sandboxId !== "") {
      return new MalformedFrameError(
        `${DataFrameKind[kind]} must use stream_id 0 without a sandbox_id`,
      );
    }
    return undefined;
  }
  if (frame.streamId === CONNECTION_STREAM_ID) {
    return new MalformedFrameError(
      `${DataFrameKind[kind]} must not use stream_id 0`,
    );
  }
  return undefined;
}

/** Encodes a frame with its varint length prefix. */
export function encodeFrame(frame: DataFrame): Uint8Array {
  const body = toBinary(DataFrameSchema, frame);
  if (body.length > MAX_FRAME_SIZE) {
    throw new FrameCodecError(
      `frame of ${body.length} bytes exceeds the frame size limit`,
    );
  }
  const prefix: number[] = [];
  let n = body.length;
  while (n >= 0x80) {
    prefix.push((n & 0x7f) | 0x80);
    n >>>= 7;
  }
  prefix.push(n);
  const out = new Uint8Array(prefix.length + body.length);
  out.set(prefix, 0);
  out.set(body, prefix.length);
  return out;
}

/** Encodes a frame and splits it into data channel messages. */
export function encodeMessages(frame: DataFrame): Uint8Array[] {
  const stream = encodeFrame(frame);
  const messages: Uint8Array[] = [];
  for (let i = 0; i < stream.length; i += MAX_DATA_CHANNEL_MESSAGE_SIZE) {
    messages.push(stream.subarray(i, i + MAX_DATA_CHANNEL_MESSAGE_SIZE));
  }
  return messages;
}

/**
 * Reassembles frames from data channel messages. It rejects a declared
 * length above MAX_FRAME_SIZE before it buffers the body and stays failed
 * after any error.
 */
export class FrameDecoder {
  #buffer = new Uint8Array(0);
  #failed: FrameCodecError | undefined;

  /** Adds one message and returns every frame that it completes. */
  push(message: Uint8Array): DataFrame[] {
    if (this.#failed) {
      throw this.#failed;
    }
    const joined = new Uint8Array(this.#buffer.length + message.length);
    joined.set(this.#buffer, 0);
    joined.set(message, this.#buffer.length);
    this.#buffer = joined;
    const frames: DataFrame[] = [];
    try {
      for (;;) {
        const header = readVarint(this.#buffer);
        if (header === undefined) {
          break;
        }
        const [length, used] = header;
        if (length > MAX_FRAME_SIZE) {
          throw new FrameCodecError(
            "declared frame length exceeds the frame size limit",
          );
        }
        if (this.#buffer.length < used + length) {
          break;
        }
        const body = this.#buffer.subarray(used, used + length);
        let frame: DataFrame;
        try {
          frame = fromBinary(DataFrameSchema, body);
        } catch {
          throw new FrameCodecError("frame body is not a valid DataFrame");
        }
        frames.push(frame);
        this.#buffer = this.#buffer.slice(used + length);
      }
    } catch (error) {
      this.#failed =
        error instanceof FrameCodecError
          ? error
          : new FrameCodecError(String(error));
      throw this.#failed;
    }
    return frames;
  }
}

function readVarint(buf: Uint8Array): [number, number] | undefined {
  let value = 0;
  let shift = 0;
  for (let i = 0; i < buf.length; i++) {
    if (i >= MAX_VARINT_LENGTH) {
      throw new FrameCodecError("frame length prefix is too long");
    }
    const byte = buf[i]!;
    // Lengths above 2^53 are rejected by the size check, so floating point
    // arithmetic is exact for every accepted value.
    value += (byte & 0x7f) * 2 ** shift;
    shift += 7;
    if ((byte & 0x80) === 0) {
      return [value, i + 1];
    }
  }
  if (buf.length >= MAX_VARINT_LENGTH) {
    throw new FrameCodecError("frame length prefix is too long");
  }
  return undefined;
}
