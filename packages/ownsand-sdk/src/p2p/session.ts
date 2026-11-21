// SPDX-License-Identifier: Apache-2.0
import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  encodeMessages,
  FrameCodecError,
  FrameDecoder,
  MAX_FRAME_SIZE,
  validateFrame,
} from "../codec.ts";
import {
  connectErrorFromDetail,
  InvalidFrameError,
  OwnSandError,
  PeerStreamOverrunError,
} from "../errors.ts";
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

/**
 * `OpenSession` reported `failed_precondition`: the sandbox is not `RUNNING`
 * (for example `PAUSED`, where an `AttachExec` still works on the relay). The
 * operation uses the relay for this attempt and the handle stays on
 * peer-to-peer, so the next operation calls `OpenSession` again. This is
 * neither a `PeerUnavailableError` (that moves the handle to the relay for
 * good) nor an authorization error (that ends the operation). `cause` is the
 * `ConnectError` that a forced peer-to-peer transport returns to the caller.
 */
export class SessionNotOpenError extends OwnSandError {
  override name = "SessionNotOpenError";
  declare readonly cause: ConnectError;

  constructor(cause: ConnectError) {
    super("OpenSession reported a sandbox that is not running", { cause });
  }
}

/** Connect codes of an `OpenSession` failure that end the operation. */
const AUTHORIZATION_CODES: ReadonlySet<Code> = new Set([
  Code.Unauthenticated,
  Code.PermissionDenied,
  Code.NotFound,
  Code.InvalidArgument,
]);

/** Largest data channel send buffer before the session waits. */
const SEND_BUFFER_HIGH = 1 << 20;
const SEND_BUFFER_LOW = 256 << 10;

/**
 * Most frames that one stream queues for a consumer that is not ready, as in
 * the Go SDK.
 */
export const STREAM_QUEUE_MAX_FRAMES = 64;
/**
 * Most payload bytes (file chunk and exec output data) that one stream
 * queues. It is at least one maximum frame, so a single frame never fails a
 * stream whose consumer is caught up.
 */
export const STREAM_QUEUE_MAX_BYTES = 8 << 20;
if (STREAM_QUEUE_MAX_BYTES < MAX_FRAME_SIZE) {
  throw new Error("the stream queue byte limit is below one frame");
}

/** The bytes of file or output data that a frame adds to a stream queue. */
function queuedBytesOf(frame: DataFrame): number {
  switch (frame.payload.case) {
    case "fileReadEvent": {
      const event = frame.payload.value.event;
      return event.case === "chunk" ? event.value.data.length : 0;
    }
    case "execEvent": {
      const event = frame.payload.value.event;
      return event.case === "stdout" || event.case === "stderr"
        ? event.value.data.length
        : 0;
    }
    default:
      return 0;
  }
}

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
  /** Tail of the send queue; a frame sends after the previous one finished. */
  #sendTail: Promise<void> = Promise.resolve();

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
   * candidates (10 seconds at most), `OpenSession`, and the data channel
   * open within 10 seconds after the answer is applied. `OpenSession` has no
   * limit of its own here, so a slow signaling call does not use up the time
   * that the channel needs to open.
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
      // Listens from now on, so an `open` event that fires before the answer
      // is applied still counts. It never rejects; the 10 second limit
      // starts only after the answer is applied (below).
      const opened = channelOpened(channel);
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
        if (signal?.aborted) {
          // A cancelled call is not an OpenSession failure: a transport can
          // report the abort as Canceled, which must not move the handle to
          // the relay.
          throw error;
        }
        if (
          error instanceof ConnectError &&
          AUTHORIZATION_CODES.has(error.code)
        ) {
          throw error;
        }
        if (
          error instanceof ConnectError &&
          error.code === Code.FailedPrecondition
        ) {
          throw new SessionNotOpenError(error);
        }
        throw new PeerUnavailableError("OpenSession failed", { cause: error });
      }
      // The answer carries the agent's DTLS fingerprint; the WebRTC stack
      // accepts only that fingerprint for the data channel.
      await pc.setRemoteDescription({ type: "answer", sdp: answer });
      await waitForOpen(opened, ctx, signal);
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
        error instanceof SessionNotOpenError ||
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
        signal,
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

  /**
   * Sends one frame. The data channel carries one length-prefixed byte
   * stream, so the messages of two frames must never interleave. A frame
   * spans many messages and waits for the send buffer between them; the
   * sends of concurrent requests therefore run one after another, and each
   * holds the turn for all messages of its frame. A caller can abort while
   * its frame waits for the turn, so the turn re-checks the signal before it
   * sends the first message and skips a cancelled frame; a frame that is
   * already in progress always completes, because a partial frame would
   * corrupt the byte stream.
   */
  async #send(
    frame: DataFrame,
    signal: AbortSignal | undefined,
  ): Promise<void> {
    // Encode before the turn so that the critical section is only I/O.
    const messages = encodeMessages(frame);
    const previous = this.#sendTail;
    const turn = previous.then(() => {
      throwIfAborted(signal);
      return this.#writeMessages(messages, signal);
    });
    // The next sender waits for this turn to finish, whether it failed or
    // not; the failure itself reaches only the caller of this frame.
    this.#sendTail = turn.catch(() => {});
    await turn;
  }

  async #writeMessages(
    messages: Uint8Array[],
    signal: AbortSignal | undefined,
  ): Promise<void> {
    for (const [index, message] of messages.entries()) {
      // Only the first message may be cancelled: once it is sent, the frame
      // must complete or the byte stream would be corrupted.
      const cancellable = index === 0 ? signal : undefined;
      // The wait loops: after it wakes, another condition (a session end, an
      // abort or a buffer that filled again) can hold, so re-check before
      // every send.
      while (!this.#ended && this.#channel.bufferedAmount > SEND_BUFFER_HIGH) {
        throwIfAborted(cancellable);
        await this.#waitForBufferLow(cancellable);
      }
      if (this.#ended) {
        throw this.#ended;
      }
      throwIfAborted(cancellable);
      this.#channel.send(message);
    }
  }

  /**
   * Resolves when the send buffer drains, the channel closes or the optional
   * signal aborts.
   */
  #waitForBufferLow(signal?: AbortSignal): Promise<void> {
    return new Promise<void>((resolve) => {
      const done = () => {
        this.#channel.removeEventListener("bufferedamountlow", done);
        this.#channel.removeEventListener("close", done);
        signal?.removeEventListener("abort", done);
        resolve();
      };
      this.#channel.addEventListener("bufferedamountlow", done);
      this.#channel.addEventListener("close", done);
      signal?.addEventListener("abort", done, { once: true });
    });
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
      const invalid = validateFrame(frame);
      if (invalid !== undefined) {
        // A frame of a newer schema or one that breaks a framing rule fails
        // only its own stream, so the operation ends with the error instead
        // of waiting for a frame that it can use. On stream_id 0 and on a
        // stream that is not open there is nothing to fail, so the frame is
        // ignored. Frames that the stream already queued are delivered first.
        const queue = this.#streams.get(frame.streamId);
        if (queue !== undefined) {
          safeLog(this.#ctx.logger, {
            event: LogEvent.InvalidFrame,
            sandboxId: this.#ctx.sandboxId,
            reason: invalid.message,
          });
          queue.fail(
            new InvalidFrameError(invalid.message, { cause: invalid }),
          );
          this.#streams.delete(frame.streamId);
        }
        continue;
      }
      this.#enqueue(frame);
    }
  }

  /**
   * Queues a frame for its stream. The receive callback cannot wait for a
   * slow consumer, and the data channel cannot be paused from JavaScript, so
   * a stream that already holds the maximum frames or bytes fails at once
   * instead of waiting as the Go SDK does for 10 seconds. The overflowing
   * frame is dropped and the frames that follow on that stream find no
   * stream. The consumer first takes the frames that are queued, so every
   * delivered frame keeps its offset, and the operation continues on a new
   * stream from the delivered offsets. Other streams and the session are not
   * affected.
   */
  #enqueue(frame: DataFrame): void {
    const queue = this.#streams.get(frame.streamId);
    if (queue === undefined) {
      return;
    }
    const size = queuedBytesOf(frame);
    if (
      queue.length >= STREAM_QUEUE_MAX_FRAMES ||
      queue.bytes + size > STREAM_QUEUE_MAX_BYTES
    ) {
      safeLog(this.#ctx.logger, {
        event: LogEvent.StreamOverrun,
        sandboxId: this.#ctx.sandboxId,
        queuedFrames: queue.length,
        queuedBytes: queue.bytes,
      });
      queue.fail(
        new PeerStreamOverrunError(
          "the consumer did not keep up with the peer-to-peer stream",
        ),
      );
      this.#streams.delete(frame.streamId);
      return;
    }
    queue.push(frame, size);
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

/**
 * A promise that resolves when the channel opens. It listens from the call on
 * and never rejects, so it needs no timer and no handler before it is awaited.
 */
function channelOpened(channel: DataChannelLike): Promise<void> {
  return new Promise<void>((resolve) => {
    const onOpen = () => {
      channel.removeEventListener("open", onOpen);
      resolve();
    };
    channel.addEventListener("open", onOpen);
  });
}

/**
 * Waits for `opened` for at most `Timing.dataChannelOpenMs`, counted from the
 * call. The caller calls it right after it applied the answer, so the time
 * that gathering and `OpenSession` took does not count. An abort ends the wait
 * with the abort reason.
 */
async function waitForOpen(
  opened: Promise<void>,
  ctx: OperationContext,
  signal: AbortSignal | undefined,
): Promise<void> {
  const stop = new AbortController();
  try {
    await Promise.race([
      opened,
      ctx.clock
        .sleep(Timing.dataChannelOpenMs, anySignal(stop.signal, signal))
        .then(() => {
          throw new PeerUnavailableError("data channel did not open in time");
        }),
    ]);
  } finally {
    // Cancels the timer when the channel opened first.
    stop.abort();
  }
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
