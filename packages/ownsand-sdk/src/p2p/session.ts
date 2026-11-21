// SPDX-License-Identifier: Apache-2.0
import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { encodeMessages, FrameCodecError, FrameDecoder } from "../codec.ts";
import { connectErrorFromDetail, OwnSandError } from "../errors.ts";
import { SessionEndReason } from "../gen/delino/ownsand/v1/common_pb.ts";
import {
  type AttachExecRequest,
  type DataFrame,
  DataFrameKind,
  DataFrameSchema,
  type ExecEvent,
  type ExecRequest,
  type FileReadEvent,
  type ReadFileRequest,
  type WriteFileRequest,
  type WriteFileResponse,
} from "../gen/delino/ownsand/v1/data_frame_pb.ts";
import { LogEvent, safeLog } from "../logger.ts";
import {
  type OperationContext,
  throwIfAborted,
  Timing,
} from "../relay/context.ts";
import { AsyncQueue } from "./queue.ts";
import {
  DATA_CHANNEL_LABEL,
  type DataChannelLike,
  type PeerConnectionConstructor,
  type PeerConnectionLike,
} from "./webrtc.ts";

/** The agent ended the session with a session-end frame. */
export class SessionEndedError extends OwnSandError {
  override name = "SessionEndedError";
  readonly reason: SessionEndReason;

  constructor(reason: SessionEndReason) {
    super(`data channel session ended: ${SessionEndReason[reason]}`);
    this.reason = reason;
  }
}

/**
 * The peer-to-peer path is not usable: ICE failed, the data channel did not
 * open within 10 seconds, or it closed without a session-end frame. The
 * sandbox handle moves to the relay.
 */
export class PeerUnavailableError extends OwnSandError {
  override name = "PeerUnavailableError";
}

/** Connect codes of an `OpenSession` failure that end the operation. */
const AUTHORIZATION_CODES: ReadonlySet<Code> = new Set([
  Code.Unauthenticated,
  Code.PermissionDenied,
  Code.NotFound,
  Code.InvalidArgument,
  Code.FailedPrecondition,
]);

/** Largest data channel send buffer before the session waits. */
const SEND_BUFFER_HIGH = 1 << 20;
const SEND_BUFFER_LOW = 256 << 10;

/**
 * One peer-to-peer data channel session with the device agent for one
 * sandbox. It carries several concurrent operations distinguished by
 * `stream_id`, which the SDK chooses.
 */
export class PeerSession {
  readonly #ctx: OperationContext;
  readonly #pc: PeerConnectionLike;
  readonly #channel: DataChannelLike;
  readonly #decoder = new FrameDecoder();
  readonly #streams = new Map<bigint, AsyncQueue<DataFrame>>();
  #nextStreamId = 1n;
  #ended: Error | undefined;

  private constructor(
    ctx: OperationContext,
    pc: PeerConnectionLike,
    channel: DataChannelLike,
  ) {
    this.#ctx = ctx;
    this.#pc = pc;
    this.#channel = channel;
    channel.addEventListener("message", (event) => this.#onMessage(event));
    channel.addEventListener("close", () =>
      this.#terminate(
        new PeerUnavailableError("data channel closed without a session end"),
      ),
    );
    channel.addEventListener("error", () =>
      this.#terminate(new PeerUnavailableError("data channel failed")),
    );
  }

  /** True after the session ended or failed. */
  get closed(): boolean {
    return this.#ended !== undefined;
  }

  /**
   * Opens a session: `GetIceServers`, a non-trickle offer with all
   * candidates, `OpenSession`, and the data channel open within 10 seconds.
   */
  static async open(
    ctx: OperationContext,
    Peer: PeerConnectionConstructor,
    signal: AbortSignal | undefined,
  ): Promise<PeerSession> {
    const ice = await ctx.dataPath.getIceServers(
      { organizationId: ctx.organizationId },
      { signal },
    );
    const pc = new Peer({
      iceServers: ice.iceServers.map((server) => ({ urls: [...server.urls] })),
    });
    try {
      const channel = pc.createDataChannel(DATA_CHANNEL_LABEL, {
        ordered: true,
      });
      channel.binaryType = "arraybuffer";
      const opened = waitForEvent(channel, "open", ctx, signal);
      // Awaited below; this keeps an early timeout from being unhandled.
      opened.catch(() => {});
      await pc.setLocalDescription(await pc.createOffer());
      await waitForGathering(pc, ctx, signal);
      const offer = pc.localDescription?.sdp;
      if (offer === undefined || offer === "") {
        throw new PeerUnavailableError("no local SDP offer");
      }
      let answer: string;
      try {
        const res = await ctx.dataPath.openSession(
          {
            organizationId: ctx.organizationId,
            sandboxId: ctx.sandboxId,
            sdpOffer: offer,
          },
          { signal },
        );
        answer = res.sdpAnswer;
      } catch (error) {
        if (
          error instanceof ConnectError &&
          AUTHORIZATION_CODES.has(error.code)
        ) {
          throw error;
        }
        throw new PeerUnavailableError("OpenSession failed", { cause: error });
      }
      // The answer carries the agent's DTLS fingerprint; the WebRTC stack
      // accepts only that fingerprint for the data channel.
      await pc.setRemoteDescription({ type: "answer", sdp: answer });
      await opened;
      if (channel.readyState !== "open") {
        throw new PeerUnavailableError("data channel did not open");
      }
      channel.bufferedAmountLowThreshold = SEND_BUFFER_LOW;
      return new PeerSession(ctx, pc, channel);
    } catch (error) {
      pc.close();
      if (
        error instanceof ConnectError ||
        error instanceof PeerUnavailableError ||
        signal?.aborted
      ) {
        throw error;
      }
      throw new PeerUnavailableError("peer-to-peer setup failed", {
        cause: error,
      });
    }
  }

  /** Closes the session. */
  close(): void {
    this.#terminate(new PeerUnavailableError("session closed"));
  }

  /** Sends an exec or exec attach request and streams its events. */
  exec(
    request:
      | { case: "execRequest"; value: ExecRequest }
      | { case: "execAttachRequest"; value: AttachExecRequest },
    signal: AbortSignal | undefined,
  ): AsyncIterable<ExecEvent> {
    return this.#stream(request, signal, (frame) => {
      if (frame.payload.case !== "execEvent") {
        return undefined;
      }
      const event = frame.payload.value;
      const c = event.event.case;
      const final =
        c === "exited" ||
        c === "failed" ||
        c === "sandboxEnded" ||
        c === "paused";
      return { value: event, final };
    });
  }

  /** Sends a file read request and streams its events. */
  readFile(
    request: ReadFileRequest,
    signal: AbortSignal | undefined,
  ): AsyncIterable<FileReadEvent> {
    return this.#stream(
      { case: "fileReadRequest", value: request },
      signal,
      (frame) => {
        if (frame.payload.case !== "fileReadEvent") {
          return undefined;
        }
        const event = frame.payload.value;
        return { value: event, final: event.event.case === "end" };
      },
    );
  }

  /** Sends one file write chunk and returns its result. */
  async writeFile(
    request: WriteFileRequest,
    signal: AbortSignal | undefined,
  ): Promise<WriteFileResponse> {
    for await (const result of this.#stream(
      { case: "fileWriteChunk", value: request },
      signal,
      (frame) =>
        frame.payload.case === "fileWriteResult"
          ? { value: frame.payload.value, final: true }
          : undefined,
    )) {
      return result;
    }
    throw new PeerUnavailableError("write stream ended without a result");
  }

  async *#stream<T>(
    payload: DataFrame["payload"],
    signal: AbortSignal | undefined,
    accept: (frame: DataFrame) => { value: T; final: boolean } | undefined,
  ): AsyncGenerator<T> {
    if (this.#ended) {
      throw this.#ended;
    }
    // A caller can abort while it waits for the shared session setup. The
    // abort event already fired then, so the listener below would never see
    // it. Reject before registering a stream or sending a frame.
    throwIfAborted(signal);
    const streamId = this.#nextStreamId++;
    const queue = new AsyncQueue<DataFrame>();
    this.#streams.set(streamId, queue);
    const onAbort = () => queue.fail(signal?.reason);
    signal?.addEventListener("abort", onAbort, { once: true });
    try {
      const kind = KIND_OF_REQUEST[payload.case as string];
      if (kind === undefined) {
        throw new Error("unsupported request payload");
      }
      await this.#send(
        create(DataFrameSchema, {
          streamId,
          sandboxId: this.#ctx.sandboxId,
          kind,
          payload,
        }),
      );
      for await (const frame of queue) {
        if (frame.payload.case === "error") {
          throw connectErrorFromDetail(frame.payload.value);
        }
        const accepted = accept(frame);
        if (accepted === undefined) {
          // A frame kind of a newer schema or one that this request does not
          // expect; ignore it.
          continue;
        }
        yield accepted.value;
        if (accepted.final) {
          return;
        }
      }
      if (this.#ended) {
        throw this.#ended;
      }
    } finally {
      signal?.removeEventListener("abort", onAbort);
      this.#streams.delete(streamId);
    }
  }

  async #send(frame: DataFrame): Promise<void> {
    for (const message of encodeMessages(frame)) {
      if (this.#ended) {
        throw this.#ended;
      }
      if (this.#channel.bufferedAmount > SEND_BUFFER_HIGH) {
        await new Promise<void>((resolve) => {
          const done = () => {
            this.#channel.removeEventListener("bufferedamountlow", done);
            this.#channel.removeEventListener("close", done);
            resolve();
          };
          this.#channel.addEventListener("bufferedamountlow", done);
          this.#channel.addEventListener("close", done);
        });
      }
      this.#channel.send(message);
    }
  }

  #onMessage(event: unknown): void {
    const data = (event as { data?: unknown }).data;
    const bytes =
      data instanceof ArrayBuffer
        ? new Uint8Array(data)
        : data instanceof Uint8Array
          ? data
          : undefined;
    if (bytes === undefined) {
      this.#terminate(
        new PeerUnavailableError("non-binary data channel message"),
      );
      return;
    }
    let frames: DataFrame[];
    try {
      frames = this.#decoder.push(bytes);
    } catch (error) {
      this.#terminate(
        new PeerUnavailableError(
          error instanceof FrameCodecError ? error.message : "decode failed",
        ),
      );
      return;
    }
    for (const frame of frames) {
      if (frame.kind === DataFrameKind.SESSION_END) {
        const reason =
          frame.payload.case === "sessionEnd"
            ? frame.payload.value.reason
            : SessionEndReason.UNSPECIFIED;
        safeLog(this.#ctx.logger, {
          event: LogEvent.SessionEnded,
          sandboxId: this.#ctx.sandboxId,
          reason: SessionEndReason[reason],
        });
        this.#terminate(new SessionEndedError(reason));
        return;
      }
      this.#streams.get(frame.streamId)?.push(frame);
    }
  }

  #terminate(error: Error): void {
    if (this.#ended) {
      return;
    }
    this.#ended = error;
    for (const queue of this.#streams.values()) {
      queue.fail(error);
    }
    this.#streams.clear();
    try {
      this.#channel.close();
    } catch {
      // Already closed.
    }
    this.#pc.close();
  }
}

const KIND_OF_REQUEST: Readonly<Record<string, DataFrameKind>> = {
  execRequest: DataFrameKind.EXEC_REQUEST,
  execAttachRequest: DataFrameKind.EXEC_ATTACH_REQUEST,
  fileReadRequest: DataFrameKind.FILE_READ_REQUEST,
  fileWriteChunk: DataFrameKind.FILE_WRITE_CHUNK,
};

function waitForEvent(
  target: DataChannelLike,
  type: string,
  ctx: OperationContext,
  signal: AbortSignal | undefined,
): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    const stop = new AbortController();
    const onEvent = () => {
      stop.abort();
      target.removeEventListener(type, onEvent);
      resolve();
    };
    target.addEventListener(type, onEvent);
    ctx.clock
      .sleep(Timing.dataChannelOpenMs, anySignal(stop.signal, signal))
      .then(
        () => {
          target.removeEventListener(type, onEvent);
          reject(new PeerUnavailableError("data channel did not open in time"));
        },
        (error: unknown) => {
          if (!stop.signal.aborted) {
            target.removeEventListener(type, onEvent);
            reject(error);
          }
        },
      );
  });
}

async function waitForGathering(
  pc: PeerConnectionLike,
  ctx: OperationContext,
  signal: AbortSignal | undefined,
): Promise<void> {
  if (pc.iceGatheringState === "complete") {
    return;
  }
  await new Promise<void>((resolve, reject) => {
    const stop = new AbortController();
    const onChange = () => {
      if (pc.iceGatheringState === "complete") {
        stop.abort();
        pc.removeEventListener("icegatheringstatechange", onChange);
        resolve();
      }
    };
    pc.addEventListener("icegatheringstatechange", onChange);
    ctx.clock
      .sleep(Timing.dataChannelOpenMs, anySignal(stop.signal, signal))
      .then(
        () => {
          pc.removeEventListener("icegatheringstatechange", onChange);
          reject(new PeerUnavailableError("ICE gathering did not complete"));
        },
        (error: unknown) => {
          if (!stop.signal.aborted) {
            pc.removeEventListener("icegatheringstatechange", onChange);
            reject(error);
          }
        },
      );
  });
}

function anySignal(a: AbortSignal, b: AbortSignal | undefined): AbortSignal {
  return b === undefined ? a : AbortSignal.any([a, b]);
}
