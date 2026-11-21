// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import {
  DataPathError,
  InvalidFrameError,
  OwnSandConfigError,
} from "./errors.ts";
import { type Logger, LogEvent, type LogRecord } from "./logger.ts";
import {
  DataPathTransport,
  SessionEndReason,
} from "./gen/delino/ownsand/v1/common_pb.ts";
import { DataPathService } from "./gen/delino/ownsand/v1/data_path_pb.ts";
import {
  type DataFrame,
  DataFrameKind,
} from "./gen/delino/ownsand/v1/data_frame_pb.ts";
import { RelayService } from "./gen/delino/ownsand/v1/relay_pb.ts";
import { ExecEventType, ExecResultKind } from "./operations/exec.ts";
import {
  type FakeAgent,
  type FakeAgentPeer,
  type FakeWebRtc,
  fakeWebRtc,
} from "./testing/fake-webrtc.ts";
import {
  attachMsg,
  ev,
  execMsg,
  SANDBOX,
  setup,
  text,
} from "./testing/harness.ts";

function p2pSetup(
  agent: FakeAgent,
  options: {
    neverOpen?: boolean;
    transport?: DataPathTransport;
    logger?: Logger;
  } = {},
) {
  let rtc: FakeWebRtc | undefined;
  const h = setup((clock) => {
    rtc = fakeWebRtc(clock, agent, options);
    return {
      webrtc: { RTCPeerConnection: rtc.RTCPeerConnection },
      transport: options.transport,
      logger: options.logger,
    };
  });
  h.api.unary(DataPathService, "GetIceServers", () => ({
    iceServers: [{ urls: ["stun:stun.ownsand.api.test:3478"] }],
  }));
  h.api.unary(DataPathService, "OpenSession", (_req, call) => ({
    sessionId: `session-${call.index + 1}`,
    sdpAnswer: "v=0 fake-answer",
    expiresIn: { seconds: 3600n },
  }));
  return { h, rtc: rtc! };
}

function execEvent(
  peer: FakeAgentPeer,
  streamId: bigint,
  event: ReturnType<typeof ev.started>,
) {
  peer.send({
    streamId,
    sandboxId: SANDBOX,
    kind: DataFrameKind.EXEC_EVENT,
    payload: { case: "execEvent", value: { event } },
  });
}

describe("peer-to-peer data path", () => {
  test("runs an exec over one data channel without a relay grant", async () => {
    const { h, rtc } = p2pSetup((frame, peer) => {
      if (frame.kind === DataFrameKind.EXEC_REQUEST) {
        execEvent(peer, frame.streamId, ev.started());
        execEvent(peer, frame.streamId, ev.stdout(0, "p2p"));
        execEvent(peer, frame.streamId, ev.exited(0));
      }
    });
    const sandbox = h.client.sandbox(SANDBOX);
    const run = await h.clock.run(sandbox.run({ command: ["echo", "p2p"] }));
    expect(text(run.stdout)).toBe("p2p");
    expect(run.result.kind).toBe(ExecResultKind.Exited);
    expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
    expect(h.api.callsOf("GetIceServers")).toHaveLength(1);
    const open = h.api.callsOf("OpenSession")[0]!.request as any;
    // Non-trickle: the offer is sent after gathering completed.
    expect(open.sdpOffer).toBe("v=0 fake-offer with candidates");
    expect(rtc.connections).toEqual([
      {
        config: { iceServers: [{ urls: ["stun:stun.ownsand.api.test:3478"] }] },
        label: "ownsand-data",
        ordered: true,
      },
    ]);
    const [request] = rtc.frames;
    expect(request!.kind).toBe(DataFrameKind.EXEC_REQUEST);
    expect(request!.streamId).toBe(1n);
    expect(request!.sandboxId).toBe(SANDBOX);
    // A second operation uses the same session with a new stream ID.
    await h.clock.run(sandbox.run({ command: ["true"] }));
    expect(h.api.callsOf("OpenSession")).toHaveLength(1);
    expect(rtc.frames[1]!.streamId).toBe(2n);
  });

  test("a caller aborted while the shared session setup is pending sends no frame", async () => {
    const { h, rtc } = p2pSetup((frame, peer) => {
      if (frame.kind === DataFrameKind.EXEC_REQUEST) {
        execEvent(peer, frame.streamId, ev.started());
        execEvent(peer, frame.streamId, ev.exited(0));
      }
    });
    const sandbox = h.client.sandbox(SANDBOX);
    const controller = new AbortController();
    const reason = new Error("caller cancelled");
    const first = sandbox.run({ command: ["a"] });
    const second = sandbox
      .run({ command: ["b"], signal: controller.signal })
      .catch((e) => e);
    // Setup is pending (ICE gathering takes virtual time).
    await Promise.resolve();
    controller.abort(reason);
    const [run, error] = await h.clock.run(Promise.all([first, second]));
    expect(error).toBe(reason);
    expect(run.result.kind).toBe(ExecResultKind.Exited);
    expect(rtc.frames).toHaveLength(1);
    expect(h.api.callsOf("OpenSession")).toHaveLength(1);
  });

  test("two callers with live signals both send a frame on the shared session", async () => {
    const { h, rtc } = p2pSetup((frame, peer) => {
      if (frame.kind === DataFrameKind.EXEC_REQUEST) {
        execEvent(peer, frame.streamId, ev.started());
        execEvent(peer, frame.streamId, ev.exited(0));
      }
    });
    const sandbox = h.client.sandbox(SANDBOX);
    const controller = new AbortController();
    const results = await h.clock.run(
      Promise.all([
        sandbox.run({ command: ["a"] }),
        sandbox.run({ command: ["b"], signal: controller.signal }),
      ]),
    );
    expect(results.map((r) => r.result.kind)).toEqual([
      ExecResultKind.Exited,
      ExecResultKind.Exited,
    ]);
    expect(rtc.frames).toHaveLength(2);
  });

  test("falls back to the relay when the data channel does not open within 10 seconds", async () => {
    const { h } = p2pSetup(() => {}, { neverOpen: true });
    h.api.stream(RelayService, "Exec", function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.exited(0));
    });
    const sandbox = h.client.sandbox(SANDBOX);
    const result = await h.clock.run(sandbox.exec({ command: ["x"] }).wait());
    expect(result.kind).toBe(ExecResultKind.Exited);
    expect(sandbox.transport).toBe(DataPathTransport.RELAY);
    expect(h.clock.now()).toBeGreaterThanOrEqual(10_000);
    await h.clock.run(sandbox.exec({ command: ["x"] }).wait());
    expect(h.api.callsOf("OpenSession")).toHaveLength(1);
  });

  test("continues an exec on the relay with AttachExec after the channel fails, never running it again", async () => {
    const { h } = p2pSetup((frame, peer) => {
      if (frame.kind === DataFrameKind.EXEC_REQUEST) {
        execEvent(peer, frame.streamId, ev.started());
        execEvent(peer, frame.streamId, ev.stdout(0, "abc"));
        queueMicrotask(() => queueMicrotask(() => peer.fail()));
      }
    });
    h.api.stream(RelayService, "AttachExec", function* () {
      yield attachMsg(ev.accepted());
      yield attachMsg(ev.started());
      yield attachMsg(ev.stdout(3, "def"));
      yield attachMsg(ev.exited(0));
    });
    const sandbox = h.client.sandbox(SANDBOX);
    const run = await h.clock.run(sandbox.run({ command: ["x"] }));
    expect(text(run.stdout)).toBe("abcdef");
    expect(sandbox.transport).toBe(DataPathTransport.RELAY);
    expect(h.api.callsOf("Exec")).toHaveLength(0);
    expect((h.api.callsOf("AttachExec")[0]!.request as any).stdoutOffset).toBe(
      3n,
    );
  });

  test("opens a new session after a session end and continues the exec there", async () => {
    let requests = 0;
    const { h, rtc } = p2pSetup((frame, peer) => {
      if (frame.kind === DataFrameKind.EXEC_REQUEST) {
        requests++;
        execEvent(peer, frame.streamId, ev.started());
        execEvent(peer, frame.streamId, ev.stdout(0, "a"));
        queueMicrotask(() =>
          queueMicrotask(() => peer.endSession(SessionEndReason.EXPIRED)),
        );
      }
      if (frame.kind === DataFrameKind.EXEC_ATTACH_REQUEST) {
        const req = (frame as DataFrame & { payload: { value: any } }).payload
          .value;
        expect(req.stdoutOffset).toBe(1n);
        execEvent(peer, frame.streamId, ev.started());
        execEvent(peer, frame.streamId, ev.stdout(1, "b"));
        execEvent(peer, frame.streamId, ev.exited(0));
      }
    });
    const sandbox = h.client.sandbox(SANDBOX);
    const run = await h.clock.run(sandbox.run({ command: ["x"] }));
    expect(text(run.stdout)).toBe("ab");
    expect(requests).toBe(1);
    expect(h.api.callsOf("OpenSession")).toHaveLength(2);
    expect(rtc.connections).toHaveLength(2);
    expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
  });

  test("ends the operation when a new session cannot be authorized", async () => {
    const { h } = p2pSetup(() => {});
    h.api.unary(DataPathService, "OpenSession", () => {
      throw new ConnectError("membership ended", Code.NotFound);
    });
    const error = await h.clock
      .run(
        h.client
          .sandbox(SANDBOX)
          .exec({ command: ["x"] })
          .wait(),
      )
      .catch((e) => e);
    expect(error.code).toBe(Code.NotFound);
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
  });

  test("a forced RELAY transport never calls OpenSession", async () => {
    const { h, rtc } = p2pSetup(() => {}, {
      transport: DataPathTransport.RELAY,
    });
    h.api.stream(RelayService, "Exec", function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.exited(0));
    });
    const sandbox = h.client.sandbox(SANDBOX);
    await h.clock.run(sandbox.exec({ command: ["x"] }).wait());
    expect(h.api.callsOf("OpenSession")).toHaveLength(0);
    expect(h.api.callsOf("GetIceServers")).toHaveLength(0);
    expect(rtc.connections).toHaveLength(0);
    expect(sandbox.transport).toBe(DataPathTransport.RELAY);
  });

  test("a forced PEER_TO_PEER transport needs a WebRTC implementation", async () => {
    const h = setup({ transport: DataPathTransport.PEER_TO_PEER });
    expect(() => h.client.sandbox(SANDBOX)).toThrow(OwnSandConfigError);
  });

  test("reads and writes files over the data channel", async () => {
    const { h } = p2pSetup((frame, peer) => {
      if (frame.payload.case === "fileReadRequest") {
        peer.send({
          streamId: frame.streamId,
          sandboxId: SANDBOX,
          kind: DataFrameKind.FILE_READ_EVENT,
          payload: {
            case: "fileReadEvent",
            value: {
              event: {
                case: "chunk",
                value: { offset: 0n, data: new TextEncoder().encode("hi") },
              },
            },
          },
        });
        peer.send({
          streamId: frame.streamId,
          sandboxId: SANDBOX,
          kind: DataFrameKind.FILE_READ_EVENT,
          payload: {
            case: "fileReadEvent",
            value: { event: { case: "end", value: { sizeBytes: 2n } } },
          },
        });
      }
      if (frame.payload.case === "fileWriteChunk") {
        const req = frame.payload.value;
        peer.send({
          streamId: frame.streamId,
          sandboxId: SANDBOX,
          kind: DataFrameKind.FILE_WRITE_RESULT,
          payload: {
            case: "fileWriteResult",
            value: {
              writeId: req.writeId,
              offset: req.offset,
              length: BigInt(req.data.length),
              finished: req.finish !== undefined,
              sizeBytes: req.finish?.sizeBytes ?? 0n,
            },
          },
        });
      }
    });
    const sandbox = h.client.sandbox(SANDBOX);
    expect(text(await h.clock.run(sandbox.readFileBytes("/a")))).toBe("hi");
    const written = await h.clock.run(sandbox.writeFile("/b", "hello"));
    expect(written.sizeBytes).toBe(5n);
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
  });

  test("an error frame ends the operation with its Connect code", async () => {
    const { h } = p2pSetup((frame, peer) => {
      peer.send({
        streamId: frame.streamId,
        sandboxId: SANDBOX,
        kind: DataFrameKind.ERROR,
        payload: {
          case: "error",
          value: { code: 9, message: "sandbox is not running" },
        },
      });
    });
    const error = await h.clock
      .run(
        h.client
          .sandbox(SANDBOX)
          .exec({ command: ["x"] })
          .wait(),
      )
      .catch((e) => e);
    expect(error.code).toBe(Code.FailedPrecondition);
  });

  describe("invalid frames", () => {
    const UNKNOWN_KIND = 99 as DataFrameKind;

    /** A valid EXEC_EVENT payload under a kind of its own choosing. */
    function execFrame(streamId: bigint, kind: DataFrameKind) {
      return {
        streamId,
        sandboxId: SANDBOX,
        kind,
        payload: {
          case: "execEvent" as const,
          value: { event: ev.stdout(1, "x") },
        },
      };
    }

    async function collect(handle: AsyncIterable<{ type: ExecEventType }>) {
      const types: ExecEventType[] = [];
      try {
        for await (const e of handle) types.push(e.type);
      } catch (error) {
        return { types, error };
      }
      return { types, error: undefined };
    }

    test("an unknown kind on an open stream ends only that operation after the earlier frames", async () => {
      const { h, rtc } = p2pSetup((frame, peer) => {
        if (frame.kind === DataFrameKind.EXEC_REQUEST) {
          execEvent(peer, frame.streamId, ev.started());
          execEvent(peer, frame.streamId, ev.stdout(0, "a"));
          peer.send(execFrame(frame.streamId, UNKNOWN_KIND));
          // A later frame of the failed stream is ignored.
          execEvent(peer, frame.streamId, ev.exited(0));
        }
      });
      const sandbox = h.client.sandbox(SANDBOX);
      const { types, error } = await h.clock.run(
        collect(sandbox.exec({ command: ["x"] })),
      );
      expect(types).toEqual([ExecEventType.Started, ExecEventType.Stdout]);
      expect(error).toBeInstanceOf(InvalidFrameError);
      expect(error).toBeInstanceOf(DataPathError);
      expect((error as InvalidFrameError).code).toBe(Code.Internal);
      expect((error as InvalidFrameError).message).toContain(
        "unknown frame kind 99",
      );
      // Terminal: no retry, no new request, no relay and no new session.
      expect(rtc.frames).toHaveLength(1);
      expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
      expect(h.api.callsOf("OpenSession")).toHaveLength(1);
    });

    test("an unspecified kind or a kind that does not match its payload ends its stream", async () => {
      const kinds = [
        DataFrameKind.UNSPECIFIED,
        DataFrameKind.ERROR,
        DataFrameKind.FILE_READ_EVENT,
      ];
      for (const kind of kinds) {
        const { h, rtc } = p2pSetup((frame, peer) => {
          if (frame.kind === DataFrameKind.EXEC_REQUEST) {
            peer.send(execFrame(frame.streamId, kind));
          }
        });
        const error = await h.clock
          .run(
            h.client
              .sandbox(SANDBOX)
              .exec({ command: ["x"] })
              .wait(),
          )
          .catch((e) => e);
        expect(error).toBeInstanceOf(InvalidFrameError);
        expect(rtc.frames).toHaveLength(1);
      }
    });

    test("a frame that breaks the stream_id rules ends its stream", async () => {
      const { h, rtc } = p2pSetup((frame, peer) => {
        if (frame.kind === DataFrameKind.EXEC_REQUEST) {
          // A connection-level kind on a stream that is open.
          peer.send({
            streamId: frame.streamId,
            kind: DataFrameKind.RELAY_KEEPALIVE,
            payload: { case: "relayKeepalive", value: {} },
          });
        }
      });
      const error = await h.clock
        .run(
          h.client
            .sandbox(SANDBOX)
            .exec({ command: ["x"] })
            .wait(),
        )
        .catch((e) => e);
      expect(error).toBeInstanceOf(InvalidFrameError);
      expect(error.message).toContain("RELAY_KEEPALIVE");
      expect(rtc.frames).toHaveLength(1);
    });

    test("a file read ends with the error after the chunks that arrived before it", async () => {
      const { h, rtc } = p2pSetup((frame, peer) => {
        if (frame.payload.case === "fileReadRequest") {
          peer.send({
            streamId: frame.streamId,
            sandboxId: SANDBOX,
            kind: DataFrameKind.FILE_READ_EVENT,
            payload: {
              case: "fileReadEvent",
              value: {
                event: {
                  case: "chunk",
                  value: { offset: 0n, data: new TextEncoder().encode("hi") },
                },
              },
            },
          });
          // An error payload under the kind of an exec event.
          peer.send({
            streamId: frame.streamId,
            sandboxId: SANDBOX,
            kind: DataFrameKind.EXEC_EVENT,
            payload: { case: "error", value: { code: 9, message: "boom" } },
          });
        }
      });
      const sandbox = h.client.sandbox(SANDBOX);
      const chunks: string[] = [];
      let error: unknown;
      await h.clock.run(
        (async () => {
          try {
            for await (const c of sandbox.readFile("/a")) {
              chunks.push(text(c));
            }
          } catch (e) {
            error = e;
          }
        })(),
      );
      expect(chunks).toEqual(["hi"]);
      expect(error).toBeInstanceOf(InvalidFrameError);
      // The payload error was not delivered as the agent's Connect error.
      expect((error as InvalidFrameError).code).toBe(Code.Internal);
      expect(rtc.frames).toHaveLength(1);
      expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
    });

    test("a file write ends at once without a retry", async () => {
      const { h, rtc } = p2pSetup((frame, peer) => {
        if (frame.payload.case === "fileWriteChunk") {
          peer.send({
            streamId: frame.streamId,
            sandboxId: SANDBOX,
            kind: UNKNOWN_KIND,
            payload: {
              case: "fileWriteResult",
              value: { writeId: "", offset: 0n, length: 0n },
            },
          });
        }
      });
      const error = await h.clock
        .run(h.client.sandbox(SANDBOX).writeFile("/b", "hello"))
        .catch((e) => e);
      expect(error).toBeInstanceOf(InvalidFrameError);
      expect(rtc.frames).toHaveLength(1);
      expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
    });

    test("other streams and the session stay usable", async () => {
      let goodStream: bigint | undefined;
      const { h, rtc } = p2pSetup((frame, peer) => {
        if (frame.payload.case !== "execRequest") return;
        const command = frame.payload.value.command[0];
        if (command === "good") {
          goodStream = frame.streamId;
          execEvent(peer, frame.streamId, ev.started());
          execEvent(peer, frame.streamId, ev.stdout(0, "ok"));
        } else if (command === "bad") {
          // The invalid frame arrives while the good stream is still open.
          peer.send(execFrame(frame.streamId, UNKNOWN_KIND));
          execEvent(peer, goodStream!, ev.exited(0));
        } else {
          execEvent(peer, frame.streamId, ev.started());
          execEvent(peer, frame.streamId, ev.exited(0));
        }
      });
      const sandbox = h.client.sandbox(SANDBOX);
      const good = sandbox.run({ command: ["good"] });
      await Promise.resolve();
      const bad = sandbox.run({ command: ["bad"] }).catch((e) => e);
      const [run, error] = await h.clock.run(Promise.all([good, bad]));
      expect(text(run.stdout)).toBe("ok");
      expect(run.result.kind).toBe(ExecResultKind.Exited);
      expect(error).toBeInstanceOf(InvalidFrameError);
      const again = await h.clock.run(sandbox.run({ command: ["next"] }));
      expect(again.result.kind).toBe(ExecResultKind.Exited);
      expect(h.api.callsOf("OpenSession")).toHaveLength(1);
      expect(rtc.connections).toHaveLength(1);
    });

    test("an invalid frame on stream 0 or on a stream that is not open is ignored", async () => {
      const { h, rtc } = p2pSetup((frame, peer) => {
        if (frame.kind === DataFrameKind.EXEC_REQUEST) {
          peer.send(execFrame(0n, UNKNOWN_KIND));
          peer.send(execFrame(0n, DataFrameKind.ERROR));
          peer.send(execFrame(77n, UNKNOWN_KIND));
          execEvent(peer, frame.streamId, ev.started());
          execEvent(peer, frame.streamId, ev.exited(0));
          // The stream finished; this frame has no open stream.
          queueMicrotask(() =>
            queueMicrotask(() =>
              peer.send(execFrame(frame.streamId, UNKNOWN_KIND)),
            ),
          );
        }
      });
      const sandbox = h.client.sandbox(SANDBOX);
      const first = await h.clock.run(sandbox.run({ command: ["a"] }));
      expect(first.result.kind).toBe(ExecResultKind.Exited);
      const second = await h.clock.run(sandbox.run({ command: ["b"] }));
      expect(second.result.kind).toBe(ExecResultKind.Exited);
      expect(h.api.callsOf("OpenSession")).toHaveLength(1);
      expect(rtc.frames).toHaveLength(2);
    });

    test("a session-end frame still ends the session, also with a framing violation", async () => {
      let requests = 0;
      const { h } = p2pSetup((frame, peer) => {
        if (frame.kind === DataFrameKind.EXEC_REQUEST) {
          requests++;
          execEvent(peer, frame.streamId, ev.started());
          // SESSION_END on stream 0 breaks no rule that stops it from ending
          // the session: it is handled before frame validation.
          peer.send({
            streamId: 0n,
            kind: DataFrameKind.SESSION_END,
            payload: {
              case: "sessionEnd",
              value: { reason: SessionEndReason.EXPIRED },
            },
          });
        }
        if (frame.kind === DataFrameKind.EXEC_ATTACH_REQUEST) {
          execEvent(peer, frame.streamId, ev.started());
          execEvent(peer, frame.streamId, ev.exited(0));
        }
      });
      const run = await h.clock.run(
        h.client.sandbox(SANDBOX).run({ command: ["x"] }),
      );
      expect(run.result.kind).toBe(ExecResultKind.Exited);
      expect(requests).toBe(1);
      expect(h.api.callsOf("OpenSession")).toHaveLength(2);
    });

    test("logs the invalid frame without frame content", async () => {
      const records: LogRecord[] = [];
      const { h } = p2pSetup(
        (frame, peer) => {
          if (frame.kind === DataFrameKind.EXEC_REQUEST) {
            peer.send(execFrame(frame.streamId, UNKNOWN_KIND));
          }
        },
        { logger: (record) => records.push(record) },
      );
      await h.clock
        .run(
          h.client
            .sandbox(SANDBOX)
            .exec({ command: ["x"] })
            .wait(),
        )
        .catch(() => {});
      const logged = records.filter((r) => r.event === LogEvent.InvalidFrame);
      expect(logged).toHaveLength(1);
      expect(logged[0]!.sandboxId).toBe(SANDBOX);
      expect(logged[0]!.reason).toContain("unknown frame kind 99");
    });
  });

  describe("slow consumers", () => {
    /** Deterministic file content, so that a gap or a duplicate shows. */
    function source(size: number): Uint8Array {
      const bytes = new Uint8Array(size);
      for (let i = 0; i < size; i++) {
        bytes[i] = (i * 31 + (i >> 8)) & 255;
      }
      return bytes;
    }

    /**
     * An agent that streams `file` from the requested offset in chunks. With
     * `paced`, it sends one frame per event loop turn, as a network does, so
     * that a consumer that keeps up can take each frame before the next.
     */
    function readAgent(
      file: Uint8Array,
      chunkSize: number,
      onExec?: (frame: DataFrame, peer: FakeAgentPeer) => void,
      paced = false,
    ): FakeAgent {
      return (frame, peer) => {
        if (frame.payload.case === "execRequest") {
          onExec?.(frame, peer);
          return;
        }
        if (frame.payload.case !== "fileReadRequest") return;
        const start = Number(frame.payload.value.offset);
        const sends: (() => void)[] = [];
        for (let at = start; at < file.length; at += chunkSize) {
          sends.push(() =>
            peer.send({
              streamId: frame.streamId,
              sandboxId: SANDBOX,
              kind: DataFrameKind.FILE_READ_EVENT,
              payload: {
                case: "fileReadEvent",
                value: {
                  event: {
                    case: "chunk",
                    value: {
                      offset: BigInt(at),
                      data: file.subarray(
                        at,
                        Math.min(at + chunkSize, file.length),
                      ),
                    },
                  },
                },
              },
            }),
          );
        }
        sends.push(() =>
          peer.send({
            streamId: frame.streamId,
            sandboxId: SANDBOX,
            kind: DataFrameKind.FILE_READ_EVENT,
            payload: {
              case: "fileReadEvent",
              value: {
                event: {
                  case: "end",
                  value: { sizeBytes: BigInt(file.length) },
                },
              },
            },
          }),
        );
        const step = (i: number) => {
          sends[i]!();
          if (i + 1 < sends.length) setImmediate(() => step(i + 1));
        };
        if (paced) step(0);
        else for (const send of sends) send();
      };
    }

    function readOffsets(rtc: FakeWebRtc): bigint[] {
      return rtc.frames.flatMap((f) =>
        f.payload.case === "fileReadRequest" ? [f.payload.value.offset] : [],
      );
    }

    function concat(parts: Uint8Array[]): Uint8Array {
      const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
      let at = 0;
      for (const part of parts) {
        out.set(part, at);
        at += part.length;
      }
      return out;
    }

    /** A promise that a test resolves by hand. */
    function gate() {
      let open!: () => void;
      const promise = new Promise<void>((resolve) => (open = resolve));
      return { promise, open };
    }

    /**
     * Reads `/big`; the consumer waits for `until` after its first chunk, as
     * a slow consumer does.
     */
    function slowRead(
      sandbox: ReturnType<FakeWebRtcHarness["h"]["client"]["sandbox"]>,
      until: Promise<void>,
    ) {
      const chunks: Uint8Array[] = [];
      const done = (async () => {
        for await (const chunk of sandbox.readFile("/big")) {
          chunks.push(chunk);
          if (chunks.length === 1) await until;
        }
        return concat(chunks);
      })();
      return { chunks, done };
    }

    type FakeWebRtcHarness = ReturnType<typeof p2pSetup>;

    test.each([
      { name: "the byte limit", size: 20 << 20, chunk: 512 << 10 },
      { name: "the frame limit", size: 200 << 10, chunk: 1 << 10 },
    ])(
      "a slow file read passes $name, fails its stream and continues from the delivered offset",
      async ({ size, chunk }) => {
        const file = source(size);
        const records: LogRecord[] = [];
        const overrun = gate();
        const p = p2pSetup(readAgent(file, chunk), {
          logger: (record) => {
            records.push(record);
            if (record.event === LogEvent.StreamOverrun) overrun.open();
          },
        });
        const { chunks, done } = slowRead(
          p.h.client.sandbox(SANDBOX),
          overrun.promise,
        );
        const bytes = await p.h.clock.run(done);
        // No gap and no duplicate: the bytes equal the source.
        expect(bytes.length).toBe(file.length);
        expect(Buffer.compare(bytes, file)).toBe(0);
        const overruns = records.filter(
          (r) => r.event === LogEvent.StreamOverrun,
        );
        expect(overruns.length).toBeGreaterThanOrEqual(1);
        for (const record of overruns) {
          // The queue never held more than its limits.
          expect(record.queuedFrames!).toBeLessThanOrEqual(64);
          expect(record.queuedBytes!).toBeLessThanOrEqual(8 << 20);
        }
        // Recovery used new streams on the same session from delivered offsets.
        const offsets = readOffsets(p.rtc);
        expect(offsets[0]).toBe(0n);
        expect(offsets.length).toBeGreaterThanOrEqual(2);
        for (let i = 1; i < offsets.length; i++) {
          expect(offsets[i]!).toBeGreaterThan(offsets[i - 1]!);
        }
        // The first stream delivered every chunk that its queue held, and the
        // next request starts right after them.
        const firstQueued = overruns[0]!.queuedFrames!;
        expect(offsets[1]! % BigInt(chunk)).toBe(0n);
        expect(offsets[1]!).toBeGreaterThanOrEqual(BigInt(firstQueued * chunk));
        expect(chunks.length).toBeGreaterThan(firstQueued);
        expect(p.h.api.callsOf("OpenSession")).toHaveLength(1);
        expect(p.h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
      },
    );

    test("a concurrent exec on the session is unaffected by a slow read", async () => {
      const file = source(2 << 20);
      const overrun = gate();
      let execPeer: FakeAgentPeer | undefined;
      let execStream = 0n;
      const p = p2pSetup(
        readAgent(file, 4 << 10, (frame, peer) => {
          execPeer = peer;
          execStream = frame.streamId;
          execEvent(peer, frame.streamId, ev.started());
          execEvent(peer, frame.streamId, ev.stdout(0, "out"));
        }),
        {
          logger: (record) => {
            if (record.event === LogEvent.StreamOverrun) overrun.open();
          },
        },
      );
      const sandbox = p.h.client.sandbox(SANDBOX);
      const exec = sandbox.run({ command: ["sleep-ish"] });
      await p.h.clock.settle();
      // The read consumer stays blocked until the exec has finished.
      const execDone = gate();
      const read = slowRead(sandbox, execDone.promise);
      await p.h.clock.run(overrun.promise);
      expect(read.chunks).toHaveLength(1);
      execEvent(execPeer!, execStream, ev.exited(0));
      const run = await p.h.clock.run(exec);
      expect(text(run.stdout)).toBe("out");
      expect(run.result.kind).toBe(ExecResultKind.Exited);
      execDone.open();
      const bytes = await p.h.clock.run(read.done);
      expect(Buffer.compare(bytes, file)).toBe(0);
      // The exec stream never failed or restarted.
      const execRequests = p.rtc.frames.filter(
        (f) => f.payload.case === "execRequest",
      );
      expect(execRequests).toHaveLength(1);
      expect(p.h.api.callsOf("OpenSession")).toHaveLength(1);
    });

    test("a consumer that keeps up never overruns", async () => {
      // 10 MiB in total: more than one stream queue holds.
      const file = source(10 << 20);
      const records: LogRecord[] = [];
      const p = p2pSetup(readAgent(file, 512 << 10, undefined, true), {
        logger: (record) => records.push(record),
      });
      const bytes = await p.h.clock.run(
        p.h.client.sandbox(SANDBOX).readFileBytes("/big"),
      );
      expect(Buffer.compare(bytes, file)).toBe(0);
      expect(records.filter((r) => r.event === LogEvent.StreamOverrun)).toEqual(
        [],
      );
      expect(readOffsets(p.rtc)).toEqual([0n]);
    });

    test("a burst below both limits is queued without failing the stream", async () => {
      // 64 frames of 1 KiB: the chunks and the end frame reach 64 frames at
      // the most, below 8 MiB.
      const file = source(63 << 10);
      const records: LogRecord[] = [];
      const wait = gate();
      const p = p2pSetup(readAgent(file, 1 << 10), {
        logger: (record) => records.push(record),
      });
      const { done } = slowRead(p.h.client.sandbox(SANDBOX), wait.promise);
      await p.h.clock.settle();
      wait.open();
      const bytes = await p.h.clock.run(done);
      expect(Buffer.compare(bytes, file)).toBe(0);
      expect(records.filter((r) => r.event === LogEvent.StreamOverrun)).toEqual(
        [],
      );
      expect(readOffsets(p.rtc)).toEqual([0n]);
    });
  });

  describe("send back-pressure", () => {
    /** Above the 1 MiB high-water mark of the session. */
    const FULL = 2 << 20;

    function writeAgent(): FakeAgent {
      return (frame, peer) => {
        if (frame.payload.case === "fileWriteChunk") {
          const req = frame.payload.value;
          peer.send({
            streamId: frame.streamId,
            sandboxId: SANDBOX,
            kind: DataFrameKind.FILE_WRITE_RESULT,
            payload: {
              case: "fileWriteResult",
              value: {
                writeId: req.writeId,
                offset: req.offset,
                length: BigInt(req.data.length),
                finished: req.finish !== undefined,
                sizeBytes: req.finish?.sizeBytes ?? 0n,
              },
            },
          });
        }
      };
    }

    /**
     * Opens the session of one sandbox handle with a small write and returns
     * the handle and its channel. Each handle owns its own session.
     */
    async function warmUp(p: ReturnType<typeof p2pSetup>) {
      const sandbox = p.h.client.sandbox(SANDBOX);
      await p.h.clock.run(sandbox.writeFile("/warm", "x"));
      expect(p.rtc.channels).toHaveLength(1);
      expect(p.rtc.frames).toHaveLength(1);
      return { sandbox, channel: p.rtc.channels[0]! };
    }

    test("concurrent multi-fragment writes never interleave their frames", async () => {
      const p = p2pSetup(writeAgent());
      const { h, rtc } = p;
      const { sandbox, channel } = await warmUp(p);
      // Every message that the SDK sends refills the buffer, so a sender
      // must wait again between the fragments of one frame.
      let messages = 0;
      channel.bufferedAmount = FULL;
      channel.afterSend = () => {
        messages++;
        channel.bufferedAmount = FULL;
      };
      // 40 KiB each: three data channel messages per frame.
      const dataA = new Uint8Array(40 << 10).fill(0xa1);
      const dataB = new Uint8Array(40 << 10).fill(0xb2);
      const a = sandbox.writeFile("/a", dataA);
      const b = sandbox.writeFile("/b", dataB);
      await h.clock.settle();
      // Back-pressure: nothing is sent while the buffer is full.
      expect(messages).toBe(0);
      expect(rtc.frames).toHaveLength(1);
      const done = Promise.all([a, b]);
      let finished = false;
      done.then(
        () => (finished = true),
        () => (finished = true),
      );
      for (let i = 0; i < 40 && !finished; i++) {
        // The buffer drains one message at a time and refills on the send.
        channel.drain();
        await h.clock.settle();
      }
      const [writtenA, writtenB] = await h.clock.run(done);
      expect(writtenA.sizeBytes).toBe(BigInt(dataA.length));
      expect(writtenB.sizeBytes).toBe(BigInt(dataB.length));
      // Both frames reached the decoder complete and in order.
      expect(messages).toBe(6);
      expect(rtc.frames).toHaveLength(3);
      const [, first, second] = rtc.frames;
      for (const frame of [first!, second!]) {
        expect(frame.payload.case).toBe("fileWriteChunk");
      }
      const chunks = [first!, second!].map(
        (frame) =>
          (frame.payload as { value: { data: Uint8Array } }).value.data,
      );
      expect(new Set([first!.streamId, second!.streamId]).size).toBe(2);
      expect(chunks[0]!.length).toBe(40 << 10);
      expect(chunks[1]!.length).toBe(40 << 10);
      expect(chunks[0]!.every((byte) => byte === chunks[0]![0])).toBe(true);
      expect(chunks[1]!.every((byte) => byte === chunks[1]![0])).toBe(true);
      expect(new Set([chunks[0]![0], chunks[1]![0]])).toEqual(
        new Set([0xa1, 0xb2]),
      );
    });

    test("a single request waits for the buffer and then completes", async () => {
      const p = p2pSetup(writeAgent());
      const { h, rtc } = p;
      const { sandbox, channel } = await warmUp(p);
      channel.bufferedAmount = FULL;
      let messages = 0;
      channel.afterSend = () => messages++;
      const write = sandbox.writeFile("/a", new Uint8Array(40 << 10).fill(7));
      await h.clock.settle();
      expect(messages).toBe(0);
      // A drained event while the buffer still reports a full queue does not
      // release the sender.
      channel.drain(FULL);
      await h.clock.settle();
      expect(messages).toBe(0);
      channel.drain(0);
      const written = await h.clock.run(write);
      expect(written.sizeBytes).toBe(BigInt(40 << 10));
      expect(messages).toBe(3);
      expect(rtc.frames).toHaveLength(2);
    });

    test("a caller aborted while its frame waits for the send turn sends nothing", async () => {
      const p = p2pSetup(writeAgent());
      const { h, rtc } = p;
      const { sandbox, channel } = await warmUp(p);
      channel.bufferedAmount = FULL;
      let messages = 0;
      channel.afterSend = () => messages++;
      // The first write holds the send turn while it waits for the buffer.
      const holder = sandbox.writeFile("/a", new Uint8Array(40 << 10).fill(7));
      await h.clock.settle();
      expect(messages).toBe(0);
      // The exec frame queues behind it; the caller aborts before its turn.
      const controller = new AbortController();
      const reason = new Error("caller cancelled");
      const cancelled = sandbox
        .run({ command: ["rm", "-rf", "/data"], signal: controller.signal })
        .catch((e) => e);
      await h.clock.settle();
      controller.abort(reason);
      await h.clock.settle();
      channel.drain(0);
      const written = await h.clock.run(holder);
      expect(written.sizeBytes).toBe(BigInt(40 << 10));
      expect(await h.clock.run(cancelled)).toBe(reason);
      // Only the three messages of the first write went out; the cancelled
      // exec request was never transmitted.
      expect(messages).toBe(3);
      expect(rtc.frames).toHaveLength(2);
      expect(
        rtc.frames.some((frame) => frame.kind === DataFrameKind.EXEC_REQUEST),
      ).toBe(false);
    });

    test("an abort during a frame in progress lets that frame finish", async () => {
      const p = p2pSetup(writeAgent());
      const { h, rtc } = p;
      const { sandbox, channel } = await warmUp(p);
      channel.bufferedAmount = FULL;
      let messages = 0;
      channel.afterSend = () => {
        messages++;
        channel.bufferedAmount = FULL;
      };
      const controller = new AbortController();
      const reason = new Error("caller cancelled");
      const write = sandbox
        .writeFile("/a", new Uint8Array(40 << 10).fill(7), {
          signal: controller.signal,
        })
        .catch((e) => e);
      await h.clock.settle();
      // The frame starts: its first message goes out, then it waits again.
      channel.drain();
      await h.clock.settle();
      expect(messages).toBe(1);
      controller.abort(reason);
      for (let i = 0; i < 10 && messages < 3; i++) {
        channel.drain();
        await h.clock.settle();
      }
      // The frame is complete on the wire even though its caller aborted, so
      // the decoder never sees a partial frame.
      expect(messages).toBe(3);
      expect(rtc.frames).toHaveLength(2);
      expect(await h.clock.run(write)).toBe(reason);
    });

    test("a session end while a sender waits stops it from sending", async () => {
      const p = p2pSetup(writeAgent());
      const { h, rtc } = p;
      const { sandbox, channel } = await warmUp(p);
      channel.bufferedAmount = FULL;
      let messages = 0;
      channel.afterSend = () => messages++;
      const waiting = sandbox
        .writeFile("/a", new Uint8Array(40 << 10))
        .catch((e) => e);
      const queued = sandbox
        .writeFile("/b", new Uint8Array(40 << 10))
        .catch((e) => e);
      await h.clock.settle();
      expect(messages).toBe(0);
      rtc.peers[0]!.endSession(SessionEndReason.UNSPECIFIED);
      await h.clock.settle();
      // The session is gone: neither sender writes to the dead channel.
      expect(messages).toBe(0);
      // Each write recovers on a new session and completes there.
      const results = await h.clock.run(Promise.all([waiting, queued]));
      for (const result of results) {
        expect(result).not.toBeInstanceOf(Error);
      }
      expect(rtc.channels).toHaveLength(2);
      expect(messages).toBe(0);
    });
  });
});
