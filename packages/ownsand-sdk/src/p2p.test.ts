// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { OwnSandConfigError } from "./errors.ts";
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
import { ExecResultKind } from "./operations/exec.ts";
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
  options: { neverOpen?: boolean; transport?: DataPathTransport } = {},
) {
  let rtc: FakeWebRtc | undefined;
  const h = setup((clock) => {
    rtc = fakeWebRtc(clock, agent, options);
    return {
      webrtc: { RTCPeerConnection: rtc.RTCPeerConnection },
      transport: options.transport,
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
});
