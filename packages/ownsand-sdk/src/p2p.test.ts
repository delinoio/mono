// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { runInNewContext } from "node:vm";
import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { encodeMessages } from "./codec.ts";
import {
  billingReasonOf,
  classifyFailure,
  DataPathError,
  FailureClass,
  InvalidFrameError,
  OwnSandConfigError,
  OwnSandCredentialError,
  OwnSandHandleClosedError,
  OwnSandValidationError,
} from "./errors.ts";
import { type Logger, LogEvent, type LogRecord } from "./logger.ts";
import {
  BillingReason,
  DataPathTransport,
  ErrorCode,
  SessionEndReason,
} from "@delino/ownsand-protos/v1/common_pb";
import { DataPathService } from "@delino/ownsand-protos/v1/data_path_pb";
import {
  type DataFrame,
  DataFrameKind,
  DataFrameSchema,
} from "@delino/ownsand-protos/v1/data_frame_pb";
import { RelayService } from "@delino/ownsand-protos/v1/relay_pb";
import { SandboxService } from "@delino/ownsand-protos/v1/sandbox_pb";
import { ExecEventType, ExecResultKind } from "./operations/exec.ts";
import { PeerUnavailableError } from "./p2p/session.ts";
import type { PeerConnectionConstructor } from "./p2p/webrtc.ts";
import type { Sandbox } from "./sandbox.ts";
import {
  type FakeAgent,
  type FakeAgentPeer,
  type FakeWebRtc,
  type FakeWebRtcOptions,
  fakeWebRtc,
} from "./testing/fake-webrtc.ts";
import { ownsandError } from "./testing/fake-api.ts";
import {
  attachMsg,
  ev,
  execMsg,
  file,
  type Harness,
  SANDBOX,
  setup,
  text,
} from "./testing/harness.ts";

function p2pSetup(
  agent: FakeAgent,
  options: FakeWebRtcOptions & {
    transport?: DataPathTransport;
    logger?: Logger;
    /** Virtual time that the OpenSession call takes. */
    openSessionMs?: number;
    /**
     * Hides the request signal from the fake OpenSession endpoint, so an abort
     * of the caller cannot cut the call short and the scripted outcome (a late
     * answer or error) really reaches the SDK after the abort. The default
     * fake rejects an aborted unary call at once, like a real fetch.
     */
    ignoreOpenSessionAbort?: boolean;
    /** Authenticates with this callback instead of the default API key. */
    accessToken?: () => string | Promise<string>;
  } = {},
) {
  let rtc: FakeWebRtc | undefined;
  const h: Harness = setup((clock) => {
    rtc = fakeWebRtc(clock, agent, options);
    return {
      webrtc: { RTCPeerConnection: rtc.RTCPeerConnection },
      transport: options.transport,
      logger: options.logger,
      ...(options.accessToken !== undefined
        ? { apiKey: undefined, accessToken: options.accessToken }
        : {}),
      ...(options.ignoreOpenSessionAbort
        ? {
            fetch: (input: Parameters<typeof fetch>[0], init?: RequestInit) =>
              h.api.fetch(
                input,
                String(
                  typeof input === "string" || input instanceof URL
                    ? input
                    : input.url,
                ).endsWith("/OpenSession")
                  ? { ...init, signal: undefined }
                  : init,
              ),
          }
        : {}),
    };
  });
  h.api.unary(DataPathService, "GetIceServers", () => ({
    iceServers: [{ urls: ["stun:stun.ownsand.api.test:3478"] }],
  }));
  h.api.unary(DataPathService, "OpenSession", async (_req, call) => {
    if (options.openSessionMs !== undefined) {
      await h.clock.sleep(options.openSessionMs);
    }
    return {
      sessionId: `session-${call.index + 1}`,
      sdpAnswer: "v=0 fake-answer",
      expiresIn: { seconds: 3600n },
    };
  });
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
  describe.each([DataPathTransport.RELAY, DataPathTransport.PEER_TO_PEER])(
    "exec validation with transport %s",
    (transport) => {
      test.each([
        ["program", { command: ["program\ud800"] }, "command"],
        ["argument", { command: ["echo", "argument\udfff"] }, "command"],
        [
          "environment key",
          { env: { "\ud800": "first", "\ufffd": "second" } },
          "env",
        ],
        ["environment value", { env: { example: "value\udfff" } }, "env"],
        [
          "working directory",
          { workingDirectory: "/tmp/\ud800" },
          "working_directory",
        ],
      ] as const)(
        "rejects a lone surrogate in the %s before transport setup",
        (_name, override, field) => {
          const { h, rtc } = p2pSetup(() => {}, { transport });
          const sandbox = h.client.sandbox(SANDBOX);
          assert.throws(
            () => sandbox.exec({ command: ["echo"], ...override }),
            {
              name: "OwnSandValidationError",
              field,
              message: `${field} must be valid UTF-8`,
            },
          );
          expect(h.api.calls).toHaveLength(0);
          expect(rtc.connections).toHaveLength(0);
        },
      );
    },
  );

  test("runs an exec over one data channel without a relay grant", async () => {
    const { h, rtc } = p2pSetup((frame, peer) => {
      if (frame.kind === DataFrameKind.EXEC_REQUEST) {
        execEvent(peer, frame.streamId, ev.started());
        execEvent(peer, frame.streamId, ev.stdout(0, "p2p"));
        execEvent(peer, frame.streamId, ev.exited(0));
      }
    });
    const sandbox = h.client.sandbox(SANDBOX);
    const input = {
      command: ["echo", "p2p 😀\ufffd"],
      env: { "key😀": "\ufffd", "key\ufffd": "😀" },
      workingDirectory: "/tmp/😀\ufffd",
    };
    const run = await h.clock.run(sandbox.run(input));
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
    expect(request!.payload).toMatchObject({
      case: "execRequest",
      value: input,
    });
    // A second operation uses the same session with a new stream ID.
    await h.clock.run(sandbox.run({ command: ["true"] }));
    expect(h.api.callsOf("OpenSession")).toHaveLength(1);
    expect(rtc.frames[1]!.streamId).toBe(2n);
  });

  test("sends the lowercase sandbox ID in frames and OpenSession for an uppercase handle", async () => {
    const { h, rtc } = p2pSetup((frame, peer) => {
      if (frame.kind === DataFrameKind.EXEC_REQUEST) {
        execEvent(peer, frame.streamId, ev.started());
        execEvent(peer, frame.streamId, ev.exited(0));
      }
    });
    const sandbox = h.client.sandbox(SANDBOX.toUpperCase());
    expect(sandbox.sandboxId).toBe(SANDBOX);
    await h.clock.run(sandbox.run({ command: ["true"] }));
    const open = h.api.callsOf("OpenSession")[0]!.request as any;
    expect(open.sandboxId).toBe(SANDBOX);
    expect(rtc.frames).not.toHaveLength(0);
    for (const frame of rtc.frames) {
      expect(frame.sandboxId).toBe(SANDBOX);
    }
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
    // 100 ms of ICE gathering, then 10 seconds for the channel to open.
    expect(h.clock.now()).toBe(10_100);
    await h.clock.run(sandbox.exec({ command: ["x"] }).wait());
    expect(h.api.callsOf("OpenSession")).toHaveLength(1);
  });

  const exitingAgent: FakeAgent = (frame, peer) => {
    if (frame.kind === DataFrameKind.EXEC_REQUEST) {
      execEvent(peer, frame.streamId, ev.started());
      execEvent(peer, frame.streamId, ev.exited(0));
    }
  };

  test("starts the 10 second open limit after the answer, so a slow OpenSession keeps peer-to-peer", async () => {
    const { h, rtc } = p2pSetup(exitingAgent, { openSessionMs: 11_000 });
    const sandbox = h.client.sandbox(SANDBOX);
    const result = await h.clock.run(sandbox.exec({ command: ["x"] }).wait());
    expect(result.kind).toBe(ExecResultKind.Exited);
    expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
    // 100 ms gathering, 11 s OpenSession, 50 ms until the channel opens.
    expect(h.clock.now()).toBe(11_150);
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
    expect(rtc.frames).toHaveLength(1);
    expect(rtc.channels[0]!.readyState).toBe("open");
  });

  test("counts gathering and OpenSession separately from the open limit", async () => {
    const { h } = p2pSetup(exitingAgent, {
      gatherMs: 6_000,
      openSessionMs: 6_000,
      openDelayMs: 9_000,
    });
    const sandbox = h.client.sandbox(SANDBOX);
    await h.clock.run(sandbox.exec({ command: ["x"] }).wait());
    expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
    expect(h.clock.now()).toBe(21_000);
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
  });

  test("falls back to the relay exactly 10 seconds after the answer when the channel never opens", async () => {
    const { h, rtc } = p2pSetup(() => {}, {
      neverOpen: true,
      openSessionMs: 6_000,
    });
    h.api.stream(RelayService, "Exec", function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.exited(0));
    });
    const sandbox = h.client.sandbox(SANDBOX);
    await h.clock.run(sandbox.exec({ command: ["x"] }).wait());
    expect(sandbox.transport).toBe(DataPathTransport.RELAY);
    // 100 ms gathering and 6 s OpenSession, then the 10 second limit.
    expect(h.clock.now()).toBe(16_100);
    expect(rtc.channels[0]!.readyState).toBe("closed");
  });

  test("counts an open event that fires before the answer", async () => {
    const { h } = p2pSetup(exitingAgent, {
      openBeforeAnswer: true,
      openSessionMs: 11_000,
      openDelayMs: 60_000,
    });
    const sandbox = h.client.sandbox(SANDBOX);
    await h.clock.run(sandbox.exec({ command: ["x"] }).wait());
    expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
    expect(h.clock.now()).toBe(11_100);
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
  });

  test("ends with the abort reason and closes the connection when the caller aborts while the channel opens", async () => {
    const { h, rtc } = p2pSetup(() => {}, { neverOpen: true });
    const controller = new AbortController();
    const reason = new Error("caller cancelled");
    const sandbox = h.client.sandbox(SANDBOX);
    const pending = sandbox
      .exec({ command: ["x"], signal: controller.signal })
      .wait()
      .catch((e) => e);
    // The answer is applied at 100 ms; abort 3 seconds into the open wait.
    await h.clock.advance(3_100);
    controller.abort(reason);
    const error = await h.clock.run(pending);
    expect(error).toBe(reason);
    expect(h.clock.now()).toBe(3_100);
    expect(rtc.channels[0]!.readyState).toBe("closed");
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
  });

  test("keeps the 10 second ICE gathering limit", async () => {
    const { h } = p2pSetup(() => {}, { gatherMs: 10_500 });
    h.api.stream(RelayService, "Exec", function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.exited(0));
    });
    const sandbox = h.client.sandbox(SANDBOX);
    await h.clock.run(sandbox.exec({ command: ["x"] }).wait());
    expect(sandbox.transport).toBe(DataPathTransport.RELAY);
    expect(h.clock.now()).toBe(10_000);
    expect(h.api.callsOf("OpenSession")).toHaveLength(0);
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

  test("keeps the shared session setup running when the first caller aborts and another caller waits", async () => {
    const { h, rtc } = p2pSetup(exitingAgent);
    const sandbox = h.client.sandbox(SANDBOX);
    const controller = new AbortController();
    const reason = new Error("first caller cancelled");
    // The first caller starts the shared setup with its own signal.
    let abortedAt = -1;
    const first = sandbox
      .run({ command: ["a"], signal: controller.signal })
      .catch((e) => {
        abortedAt = h.clock.now();
        return e;
      });
    const second = sandbox.run({ command: ["b"] });
    // Setup is pending (ICE gathering takes virtual time).
    await Promise.resolve();
    controller.abort(reason);
    const [error, run] = await h.clock.run(Promise.all([first, second]));
    // The aborted caller ends promptly with its own reason...
    expect(error).toBe(reason);
    expect(abortedAt).toBe(0);
    // ...and the other caller still gets the shared session.
    expect(run.result.kind).toBe(ExecResultKind.Exited);
    expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
    expect(h.api.callsOf("OpenSession")).toHaveLength(1);
    expect(rtc.connections).toHaveLength(1);
    expect(rtc.channels[0]!.readyState).toBe("open");
    expect(rtc.frames).toHaveLength(1);
  });

  test("cancels the shared session setup only when every waiting caller aborted, then starts a fresh one", async () => {
    const { h, rtc } = p2pSetup(exitingAgent);
    const sandbox = h.client.sandbox(SANDBOX);
    const a = new AbortController();
    const b = new AbortController();
    const reasonA = new Error("a cancelled");
    const reasonB = new Error("b cancelled");
    const first = sandbox
      .run({ command: ["a"], signal: a.signal })
      .catch((e) => e);
    const second = sandbox
      .run({ command: ["b"], signal: b.signal })
      .catch((e) => e);
    await h.clock.advance(50);
    a.abort(reasonA);
    await h.clock.advance(20);
    // One caller still waits, so the setup is still running.
    expect(rtc.channels[0]!.readyState).not.toBe("closed");
    b.abort(reasonB);
    const errors = await h.clock.run(Promise.all([first, second]));
    expect(errors).toEqual([reasonA, reasonB]);
    // Nothing is left to wait for the setup: it was cancelled before the
    // offer was sent, and the connection is closed.
    expect(h.api.callsOf("OpenSession")).toHaveLength(0);
    expect(rtc.channels[0]!.readyState).toBe("closed");
    expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
    // A later caller starts a new setup.
    const run = await h.clock.run(sandbox.run({ command: ["c"] }));
    expect(run.result.kind).toBe(ExecResultKind.Exited);
    expect(rtc.connections).toHaveLength(2);
    expect(h.api.callsOf("OpenSession")).toHaveLength(1);
    expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
  });

  test("an abort during OpenSession keeps the handle on peer-to-peer and closes the connection", async () => {
    const { h, rtc } = p2pSetup(exitingAgent, { openSessionMs: 5_000 });
    const sandbox = h.client.sandbox(SANDBOX);
    const controller = new AbortController();
    const reason = new Error("caller cancelled");
    const pending = sandbox
      .run({ command: ["a"], signal: controller.signal })
      .catch((e) => e);
    // 100 ms of gathering, then OpenSession is in flight.
    await h.clock.advance(1_000);
    expect(h.api.callsOf("OpenSession")).toHaveLength(1);
    controller.abort(reason);
    const error = await h.clock.run(pending);
    expect(error).toBe(reason);
    expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
    expect(rtc.channels[0]!.readyState).toBe("closed");
    // The abort was not a peer failure: the next operation uses the peer
    // path with a fresh session.
    const run = await h.clock.run(sandbox.run({ command: ["b"] }));
    expect(run.result.kind).toBe(ExecResultKind.Exited);
    expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
    expect(rtc.connections).toHaveLength(2);
  });

  test("a Canceled error from OpenSession after an abort is not a peer failure", async () => {
    const { h, rtc } = p2pSetup(exitingAgent);
    h.api.unary(DataPathService, "OpenSession", async (_req, call) => {
      await h.clock.sleep(5_000);
      if (call.index === 0) {
        // A transport reports an abort as Canceled, not as the abort reason.
        throw new ConnectError("canceled", Code.Canceled);
      }
      return {
        sessionId: `session-${call.index + 1}`,
        sdpAnswer: "v=0 fake-answer",
        expiresIn: { seconds: 3600n },
      };
    });
    const sandbox = h.client.sandbox(SANDBOX);
    const controller = new AbortController();
    const reason = new Error("caller cancelled");
    const pending = sandbox
      .run({ command: ["a"], signal: controller.signal })
      .catch((e) => e);
    await h.clock.advance(1_000);
    controller.abort(reason);
    expect(await h.clock.run(pending)).toBe(reason);
    // Let the abandoned OpenSession answer with Canceled.
    await h.clock.advance(10_000);
    expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
    expect(rtc.channels[0]!.readyState).toBe("closed");
  });

  test("a late session end from a retired session keeps the replacement session", async () => {
    const peers: FakeAgentPeer[] = [];
    const { h, rtc } = p2pSetup((frame, peer) => {
      if (frame.kind === DataFrameKind.EXEC_REQUEST) {
        const request = (frame as DataFrame & { payload: { value: any } })
          .payload.value;
        execEvent(peer, frame.streamId, ev.started());
        if (request.command[0] === "a") {
          // Command "a" keeps running until its session ends.
          execEvent(peer, frame.streamId, ev.stdout(0, "a"));
          peers.push(peer);
        } else {
          execEvent(peer, frame.streamId, ev.exited(0));
        }
      }
      if (frame.kind === DataFrameKind.EXEC_ATTACH_REQUEST) {
        execEvent(peer, frame.streamId, ev.started());
        execEvent(peer, frame.streamId, ev.exited(0));
      }
    });
    const sandbox = h.client.sandbox(SANDBOX);
    // Consumer one starts on session one and then stops reading.
    const first = sandbox.exec({ command: ["a"] });
    const events = first[Symbol.asyncIterator]();
    expect((await h.clock.run(events.next())).done).toBe(false);
    expect(peers).toHaveLength(1);
    expect(h.api.callsOf("OpenSession")).toHaveLength(1);
    // The agent ends session one while consumer one is not reading.
    peers[0]!.endSession(SessionEndReason.EXPIRED);
    await h.clock.advance(10);
    // A second call replaces the session.
    await h.clock.run(sandbox.exec({ command: ["b"] }).wait());
    expect(h.api.callsOf("OpenSession")).toHaveLength(2);
    // Consumer one resumes and reports the end of the retired session. It
    // continues on the replacement and opens no third session.
    await h.clock.run(
      (async () => {
        for (;;) {
          const next = await events.next();
          if (next.done) return;
        }
      })(),
    );
    expect(h.api.callsOf("OpenSession")).toHaveLength(2);
    expect(rtc.connections).toHaveLength(2);
    expect(rtc.channels[1]!.readyState).toBe("open");
    // A third operation still uses the replacement.
    await h.clock.run(sandbox.run({ command: ["c"] }));
    expect(h.api.callsOf("OpenSession")).toHaveLength(2);
    expect(rtc.connections).toHaveLength(2);
    expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
  });

  test("rejects a negative file offset and a bad attach exec request before any session or relay call", () => {
    const { h, rtc } = p2pSetup(exitingAgent);
    const sandbox = h.client.sandbox(SANDBOX);
    const attempts: [() => unknown, string][] = [
      [() => sandbox.readFile("/f", { offset: -1n }), "offset"],
      [() => sandbox.attachExec("", {}), "exec_id"],
      [
        () =>
          sandbox.attachExec("0192f4b6-8000-7000-8000-000000000001", {
            stdoutOffset: -1n,
          }),
        "stdout_offset",
      ],
      [
        () =>
          sandbox.attachExec("0192f4b6-8000-7000-8000-000000000001", {
            stderrOffset: -1n,
          }),
        "stderr_offset",
      ],
    ];
    for (const [attempt, field] of attempts) {
      let error: unknown;
      try {
        attempt();
      } catch (e) {
        error = e;
      }
      expect(error).toBeInstanceOf(OwnSandValidationError);
      expect((error as OwnSandValidationError).field).toBe(field);
    }
    expect(h.api.calls).toHaveLength(0);
    expect(rtc.connections).toHaveLength(0);
  });

  describe.each([
    { mode: "auto", transport: undefined },
    { mode: "forced peer", transport: DataPathTransport.PEER_TO_PEER },
  ])("local peer setup credentials ($mode)", ({ transport }) => {
    const token = "osk_test_0123456789abcdef";

    describe.each([
      { rpc: "GetIceServers", iceCalls: 0 },
      { rpc: "OpenSession", iceCalls: 1 },
    ])("$rpc", ({ iceCalls }) => {
      test.each([
        {
          failure: "throws",
          accessToken: () => {
            throw new Error("token source failed");
          },
        },
        {
          failure: "rejects",
          accessToken: () => Promise.reject(new Error("token source failed")),
        },
        { failure: "returns an empty token", accessToken: () => "" },
      ])(
        "keeps peer routing when the callback $failure",
        async ({ accessToken }) => {
          let tokenCalls = 0;
          const { h, rtc } = p2pSetup(exitingAgent, {
            transport,
            accessToken: () =>
              ++tokenCalls === iceCalls + 1 ? accessToken() : token,
          });
          const sandbox = h.client.sandbox(SANDBOX);
          const error = await h.clock
            .run(sandbox.run({ command: ["x"] }))
            .catch((error: unknown) => error);

          expect(error).toBeInstanceOf(ConnectError);
          expect((error as ConnectError).cause).toBeInstanceOf(
            OwnSandCredentialError,
          );
          expect(classifyFailure(error)).toBe(FailureClass.Terminal);
          expect(h.api.callsOf("GetIceServers")).toHaveLength(iceCalls);
          expect(h.api.callsOf("OpenSession")).toHaveLength(0);
          expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
          expect(rtc.connections).toHaveLength(iceCalls);
          expect(rtc.closedConnections).toBe(iceCalls);
          expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);

          const run = await h.clock.run(sandbox.run({ command: ["x"] }));
          expect(run.result.kind).toBe(ExecResultKind.Exited);
          expect(h.api.callsOf("GetIceServers")).toHaveLength(iceCalls + 1);
          expect(h.api.callsOf("OpenSession")).toHaveLength(1);
          expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
          expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
        },
      );
    });
  });

  test("retries peer setup after a local token failure following a session end", async () => {
    let peer: FakeAgentPeer | undefined;
    let tokenCalls = 0;
    const { h, rtc } = p2pSetup(
      (frame, activePeer) => {
        peer = activePeer;
        exitingAgent(frame, activePeer);
      },
      {
        accessToken: () =>
          ++tokenCalls === 3 ? "" : "osk_test_0123456789abcdef",
      },
    );
    const sandbox = h.client.sandbox(SANDBOX);
    await h.clock.run(sandbox.run({ command: ["x"] }));
    peer!.endSession(SessionEndReason.EXPIRED);
    await h.clock.settle();

    const error = await h.clock
      .run(sandbox.run({ command: ["x"] }))
      .catch((error: unknown) => error);
    expect(error).toBeInstanceOf(ConnectError);
    expect((error as ConnectError).cause).toBeInstanceOf(
      OwnSandCredentialError,
    );
    expect(classifyFailure(error)).toBe(FailureClass.Terminal);
    expect(h.api.callsOf("GetIceServers")).toHaveLength(1);
    expect(h.api.callsOf("OpenSession")).toHaveLength(1);
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
    expect(rtc.closedConnections).toBe(1);
    expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);

    const run = await h.clock.run(sandbox.run({ command: ["x"] }));
    expect(run.result.kind).toBe(ExecResultKind.Exited);
    expect(h.api.callsOf("GetIceServers")).toHaveLength(2);
    expect(h.api.callsOf("OpenSession")).toHaveLength(2);
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
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

  test("uses the relay once when OpenSession reports a sandbox that is not running", async () => {
    const { h, rtc } = p2pSetup((frame, peer) => {
      if (frame.kind === DataFrameKind.EXEC_REQUEST) {
        execEvent(peer, frame.streamId, ev.started());
        execEvent(peer, frame.streamId, ev.exited(0));
      }
    });
    h.api.unary(DataPathService, "OpenSession", (_req, call) => {
      if (call.index === 0) {
        throw new ConnectError(
          "sandbox is not running",
          Code.FailedPrecondition,
        );
      }
      return {
        sessionId: `session-${call.index + 1}`,
        sdpAnswer: "v=0 fake-answer",
        expiresIn: { seconds: 3600n },
      };
    });
    h.api.stream(RelayService, "Exec", function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.exited(0));
    });
    const sandbox = h.client.sandbox(SANDBOX);
    const result = await h.clock.run(sandbox.exec({ command: ["x"] }).wait());
    expect(result.kind).toBe(ExecResultKind.Exited);
    expect(h.api.callsOf("OpenSession")).toHaveLength(1);
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(1);
    expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
    // The handle stayed on peer-to-peer: the next operation opens a session.
    await h.clock.run(sandbox.exec({ command: ["x"] }).wait());
    expect(h.api.callsOf("OpenSession")).toHaveLength(2);
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(1);
    expect(rtc.frames).toHaveLength(1);
  });

  test("a forced PEER_TO_PEER transport returns the failed_precondition of OpenSession", async () => {
    const { h } = p2pSetup(() => {}, {
      transport: DataPathTransport.PEER_TO_PEER,
    });
    h.api.unary(DataPathService, "OpenSession", () => {
      throw new ConnectError("sandbox is not running", Code.FailedPrecondition);
    });
    const error = await h.clock
      .run(
        h.client
          .sandbox(SANDBOX)
          .exec({ command: ["x"] })
          .wait(),
      )
      .catch((e) => e);
    expect(error).toBeInstanceOf(ConnectError);
    expect(error.code).toBe(Code.FailedPrecondition);
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
  });

  test("moves the handle to the relay when OpenSession fails with another error", async () => {
    const { h } = p2pSetup(() => {});
    h.api.unary(DataPathService, "OpenSession", () => {
      throw new ConnectError("agent unreachable", Code.Unavailable);
    });
    h.api.stream(RelayService, "Exec", function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.exited(0));
    });
    const sandbox = h.client.sandbox(SANDBOX);
    await h.clock.run(sandbox.exec({ command: ["x"] }).wait());
    expect(sandbox.transport).toBe(DataPathTransport.RELAY);
    await h.clock.run(sandbox.exec({ command: ["x"] }).wait());
    expect(h.api.callsOf("OpenSession")).toHaveLength(1);
  });

  test("moves the handle to the relay when the agent reports its session capacity as resource_exhausted", async () => {
    const { h } = p2pSetup(() => {});
    h.api.unary(DataPathService, "OpenSession", () => {
      throw new ConnectError(
        "agent peer-to-peer session capacity reached",
        Code.ResourceExhausted,
      );
    });
    h.api.stream(RelayService, "Exec", function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.exited(0));
    });
    const sandbox = h.client.sandbox(SANDBOX);
    const result = await h.clock.run(sandbox.exec({ command: ["x"] }).wait());
    expect(result.kind).toBe(ExecResultKind.Exited);
    expect(sandbox.transport).toBe(DataPathTransport.RELAY);
    expect(h.api.callsOf("OpenSession")).toHaveLength(1);
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(1);
  });

  test("a forced PEER_TO_PEER transport reports a resource_exhausted OpenSession as unavailable", async () => {
    const { h } = p2pSetup(() => {}, {
      transport: DataPathTransport.PEER_TO_PEER,
    });
    h.api.unary(DataPathService, "OpenSession", () => {
      throw new ConnectError(
        "agent peer-to-peer session capacity reached",
        Code.ResourceExhausted,
      );
    });
    const error = await h.clock
      .run(
        h.client
          .sandbox(SANDBOX)
          .exec({ command: ["x"] })
          .wait(),
      )
      .catch((e) => e);
    // A forced PEER_TO_PEER handle reports a setup failure as an unavailable
    // data path error and never asks for a relay grant.
    expect(error).toBeInstanceOf(DataPathError);
    expect((error as DataPathError).code).toBe(Code.Unavailable);
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
  });

  describe("when the WebRTC constructor throws", () => {
    /** A peer connection constructor that throws, with the throw recorded. */
    function throwingWebRtc(
      error: Error,
      onConstruct: () => void = () => {},
    ): { RTCPeerConnection: PeerConnectionConstructor; constructed: number[] } {
      const constructed: number[] = [];
      return {
        constructed,
        RTCPeerConnection: class {
          constructor() {
            constructed.push(constructed.length);
            onConstruct();
            throw error;
          }
        } as unknown as PeerConnectionConstructor,
      };
    }

    function throwingSetup(
      webrtc: { RTCPeerConnection: PeerConnectionConstructor },
      transport?: DataPathTransport,
    ) {
      const h = setup({ webrtc, transport });
      h.api.unary(DataPathService, "GetIceServers", () => ({
        iceServers: [{ urls: ["stun:stun.ownsand.api.test:3478"] }],
      }));
      h.api.unary(DataPathService, "OpenSession", () => ({
        sessionId: "session-1",
        sdpAnswer: "v=0 fake-answer",
        expiresIn: { seconds: 3600n },
      }));
      h.api.stream(RelayService, "Exec", function* () {
        yield execMsg(ev.accepted());
        yield execMsg(ev.exited(0));
      });
      return h;
    }

    test("an automatic transport uses the relay and keeps the handle on it", async () => {
      const webrtc = throwingWebRtc(new Error("native webrtc unavailable"));
      const h = throwingSetup(webrtc);
      const sandbox = h.client.sandbox(SANDBOX);
      const result = await h.clock.run(sandbox.exec({ command: ["x"] }).wait());
      expect(result.kind).toBe(ExecResultKind.Exited);
      expect(sandbox.transport).toBe(DataPathTransport.RELAY);
      expect(webrtc.constructed).toHaveLength(1);
      expect(h.api.callsOf("GetIceServers")).toHaveLength(1);
      expect(h.api.callsOf("OpenSession")).toHaveLength(0);
      expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(1);
      // The handle stays on the relay: no later operation builds a peer.
      await h.clock.run(sandbox.exec({ command: ["x"] }).wait());
      expect(webrtc.constructed).toHaveLength(1);
      expect(h.api.callsOf("GetIceServers")).toHaveLength(1);
      expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(2);
    });

    test("a forced PEER_TO_PEER transport returns a data path error with the original cause", async () => {
      const original = new Error("native webrtc unavailable");
      const webrtc = throwingWebRtc(original);
      const h = throwingSetup(webrtc, DataPathTransport.PEER_TO_PEER);
      const error = await h.clock
        .run(
          h.client
            .sandbox(SANDBOX)
            .exec({ command: ["x"] })
            .wait(),
        )
        .catch((e) => e);
      expect(error).toBeInstanceOf(DataPathError);
      expect(error.code).toBe(Code.Unavailable);
      expect(error.cause).toBeInstanceOf(PeerUnavailableError);
      expect(error.cause.message).toBe("peer-to-peer setup failed");
      expect(error.cause.cause).toBe(original);
      expect(h.api.callsOf("OpenSession")).toHaveLength(0);
      expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
    });

    test("a caller abort ends with the abort reason and starts no relay", async () => {
      const controller = new AbortController();
      const reason = new Error("caller cancelled");
      const webrtc = throwingWebRtc(
        new Error("native webrtc unavailable"),
        () => controller.abort(reason),
      );
      const h = throwingSetup(webrtc);
      const sandbox = h.client.sandbox(SANDBOX);
      const error = await h.clock
        .run(sandbox.exec({ command: ["x"], signal: controller.signal }).wait())
        .catch((e) => e);
      expect(error).toBe(reason);
      expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
      expect(h.api.callsOf("OpenSession")).toHaveLength(0);
      expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
    });
  });

  for (const code of [
    Code.Unauthenticated,
    Code.PermissionDenied,
    Code.InvalidArgument,
  ]) {
    test(`ends the operation when OpenSession fails with ${Code[code]}`, async () => {
      const { h } = p2pSetup(() => {});
      h.api.unary(DataPathService, "OpenSession", () => {
        throw new ConnectError("rejected", code);
      });
      const error = await h.clock
        .run(
          h.client
            .sandbox(SANDBOX)
            .exec({ command: ["x"] })
            .wait(),
        )
        .catch((e) => e);
      expect(error.code).toBe(code);
      expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
    });
  }

  for (const transport of [undefined, DataPathTransport.PEER_TO_PEER]) {
    test(`ends the operation without the relay when OpenSession fails with a billing error (transport ${transport === undefined ? "auto" : DataPathTransport[transport]})`, async () => {
      const { h } = p2pSetup(() => {}, { transport });
      h.api.unary(DataPathService, "OpenSession", () => {
        throw ownsandError(
          Code.ResourceExhausted,
          "suspended; see https://accounts.delino.io",
          { billingReason: BillingReason.SUSPENDED },
        );
      });
      const error = await h.clock
        .run(
          h.client
            .sandbox(SANDBOX)
            .exec({ command: ["x"] })
            .wait(),
        )
        .catch((e) => e);
      expect(billingReasonOf(error)).toBe(BillingReason.SUSPENDED);
      expect(h.api.callsOf("OpenSession")).toHaveLength(1);
      expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
    });
  }

  describe("binary data channel messages of any realm", () => {
    /** A Uint8Array that a `node:vm` context built, so `instanceof` misses it. */
    const foreignBytes = (bytes: Uint8Array): Uint8Array =>
      runInNewContext("(src) => Uint8Array.from(src)")(bytes) as Uint8Array;
    /** A view of `bytes` inside a larger foreign buffer with sentinel bytes. */
    const foreignView = (bytes: Uint8Array): Uint8Array => {
      const padded = foreignBytes(
        new Uint8Array([
          ...Array(7).fill(0xee),
          ...bytes,
          ...Array(5).fill(0xdd),
        ]),
      );
      return padded.subarray(7, 7 + bytes.length);
    };
    const localView = (bytes: Uint8Array): Uint8Array => {
      const padded = new Uint8Array([
        ...Array(7).fill(0xee),
        ...bytes,
        ...Array(5).fill(0xdd),
      ]);
      return padded.subarray(7, 7 + bytes.length);
    };

    const valid: [string, (bytes: Uint8Array) => unknown][] = [
      ["a same-realm ArrayBuffer", (b) => b.slice().buffer],
      ["a same-realm Uint8Array", (b) => b.slice()],
      ["a same-realm Uint8Array view", localView],
      ["a cross-realm ArrayBuffer", (b) => foreignBytes(b).buffer],
      ["a cross-realm Uint8Array", foreignBytes],
      ["a cross-realm Uint8Array view", foreignView],
    ];
    const invalid: [string, (bytes: Uint8Array) => unknown][] = [
      ["a string", () => "not binary"],
      ["a same-realm DataView", (b) => new DataView(b.slice().buffer)],
      [
        "a cross-realm DataView",
        (b) =>
          runInNewContext("(src) => new DataView(Uint8Array.from(src).buffer)")(
            b,
          ),
      ],
    ];

    /** An agent that answers an exec with raw messages that `wrap` builds. */
    function rawAgent(
      channelOf: () => { receive(data: unknown): void },
      wrap: (bytes: Uint8Array) => unknown,
    ): FakeAgent {
      return (frame) => {
        if (frame.kind !== DataFrameKind.EXEC_REQUEST) return;
        for (const event of [ev.started(), ev.stdout(0, "ok"), ev.exited(0)]) {
          const reply = create(DataFrameSchema, {
            streamId: frame.streamId,
            sandboxId: SANDBOX,
            kind: DataFrameKind.EXEC_EVENT,
            payload: { case: "execEvent", value: { event } },
          });
          for (const message of encodeMessages(reply)) {
            // A copy, so that no message shares bytes with the encoder.
            channelOf().receive(wrap(message.slice()));
          }
        }
      };
    }

    for (const [name, wrap] of valid) {
      test(`${name} reaches the decoder and leaves the session usable`, async () => {
        const holder: { p?: ReturnType<typeof p2pSetup> } = {};
        const p = p2pSetup(
          rawAgent(() => holder.p!.rtc.channels[0]!, wrap),
          { transport: DataPathTransport.PEER_TO_PEER },
        );
        holder.p = p;
        const { h, rtc } = p;
        const sandbox = h.client.sandbox(SANDBOX);
        const first = await h.clock.run(sandbox.run({ command: ["a"] }));
        expect(text(first.stdout)).toBe("ok");
        expect(first.result.kind).toBe(ExecResultKind.Exited);
        // The same session serves the next operation.
        const second = await h.clock.run(sandbox.run({ command: ["b"] }));
        expect(text(second.stdout)).toBe("ok");
        expect(rtc.channels).toHaveLength(1);
        expect(h.api.callsOf("OpenSession")).toHaveLength(1);
        expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
      });
    }

    for (const [name, wrap] of invalid) {
      test(`${name} ends the session as a non-binary message`, async () => {
        const holder: { p?: ReturnType<typeof p2pSetup> } = {};
        const p = p2pSetup(
          rawAgent(() => holder.p!.rtc.channels[0]!, wrap),
          { transport: DataPathTransport.PEER_TO_PEER },
        );
        holder.p = p;
        const { h } = p;
        const sandbox = h.client.sandbox(SANDBOX);
        const error = await h.clock
          .run(sandbox.run({ command: ["a"] }))
          .catch((e) => e);
        expect(error).toBeInstanceOf(DataPathError);
        const cause = (error as DataPathError).cause;
        expect(cause).toBeInstanceOf(PeerUnavailableError);
        expect((cause as Error).message).toBe(
          "non-binary data channel message",
        );
        expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
      });
    }
  });

  describe("when GetIceServers fails", () => {
    const relayOperations = {
      exec: {
        install(h: ReturnType<typeof p2pSetup>["h"]) {
          h.api.stream(RelayService, "Exec", function* () {
            yield execMsg(ev.accepted());
            yield execMsg(ev.exited(0));
          });
        },
        run: (h: ReturnType<typeof p2pSetup>["h"], sandbox: Sandbox) =>
          h.clock.run(sandbox.exec({ command: ["x"] }).wait()),
      },
      readFile: {
        install(h: ReturnType<typeof p2pSetup>["h"]) {
          h.api.stream(RelayService, "ReadFile", function* () {
            yield file.accepted();
            yield file.chunk(0, "hi");
            yield file.end(2);
          });
        },
        run: (h: ReturnType<typeof p2pSetup>["h"], sandbox: Sandbox) =>
          h.clock.run(sandbox.readFileBytes("/a")),
      },
      writeFile: {
        install(h: ReturnType<typeof p2pSetup>["h"]) {
          h.api.unary(RelayService, "WriteFile", (req) => ({
            writeId: req.writeId,
            offset: req.offset,
            length: BigInt(req.data.length),
            finished: true,
            sizeBytes: req.finish?.sizeBytes ?? 0n,
          }));
        },
        run: (h: ReturnType<typeof p2pSetup>["h"], sandbox: Sandbox) =>
          h.clock.run(sandbox.writeFile("/b", "hello")),
      },
    };

    for (const code of [
      Code.Unavailable,
      Code.Internal,
      Code.DeadlineExceeded,
      Code.ResourceExhausted,
    ]) {
      for (const [name, operation] of Object.entries(relayOperations)) {
        test(`${name} completes over the relay when GetIceServers fails with ${Code[code]}`, async () => {
          const { h, rtc } = p2pSetup(() => {});
          h.api.unary(DataPathService, "GetIceServers", () => {
            throw new ConnectError("down", code);
          });
          operation.install(h);
          const sandbox = h.client.sandbox(SANDBOX);
          await operation.run(h, sandbox);
          expect(sandbox.transport).toBe(DataPathTransport.RELAY);
          expect(h.api.callsOf("GetIceServers")).toHaveLength(1);
          expect(h.api.callsOf("OpenSession")).toHaveLength(0);
          expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(1);
          expect(rtc.connections).toHaveLength(0);
          // The handle stays on the relay: no later operation calls
          // GetIceServers again.
          await operation.run(h, sandbox);
          expect(h.api.callsOf("GetIceServers")).toHaveLength(1);
          expect(h.api.callsOf("OpenSession")).toHaveLength(0);
          expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(2);
          expect(sandbox.transport).toBe(DataPathTransport.RELAY);
        });
      }
    }

    for (const code of [
      Code.Unauthenticated,
      Code.PermissionDenied,
      Code.NotFound,
      Code.InvalidArgument,
    ]) {
      test(`ends the operation when GetIceServers fails with ${Code[code]}`, async () => {
        const { h } = p2pSetup(() => {});
        h.api.unary(DataPathService, "GetIceServers", () => {
          throw new ConnectError("rejected", code);
        });
        const sandbox = h.client.sandbox(SANDBOX);
        const error = await h.clock
          .run(sandbox.exec({ command: ["x"] }).wait())
          .catch((e) => e);
        expect(error).toBeInstanceOf(ConnectError);
        expect(error.code).toBe(code);
        expect(h.api.callsOf("OpenSession")).toHaveLength(0);
        expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
        expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
      });
    }

    for (const transport of [undefined, DataPathTransport.PEER_TO_PEER]) {
      test(`ends the operation without the relay on a billing error (transport ${transport === undefined ? "auto" : DataPathTransport[transport]})`, async () => {
        const { h } = p2pSetup(() => {}, { transport });
        h.api.unary(DataPathService, "GetIceServers", () => {
          throw ownsandError(
            Code.ResourceExhausted,
            "suspended; see https://accounts.delino.io",
            { billingReason: BillingReason.SUSPENDED },
          );
        });
        const sandbox = h.client.sandbox(SANDBOX);
        const error = await h.clock
          .run(sandbox.exec({ command: ["x"] }).wait())
          .catch((e) => e);
        expect(billingReasonOf(error)).toBe(BillingReason.SUSPENDED);
        expect(h.api.callsOf("OpenSession")).toHaveLength(0);
        expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
        expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
      });
    }

    test("an abort while GetIceServers is pending ends with the abort reason and keeps peer-to-peer", async () => {
      const { h, rtc } = p2pSetup(() => {});
      h.api.unary(DataPathService, "GetIceServers", async (_req, call) => {
        await h.clock.sleep(5_000);
        if (call.signal.aborted) {
          // A transport reports the abort as Canceled, which must not move the
          // handle to the relay.
          throw new ConnectError("canceled", Code.Canceled);
        }
        return { iceServers: [{ urls: ["stun:stun.ownsand.api.test:3478"] }] };
      });
      const sandbox = h.client.sandbox(SANDBOX);
      const controller = new AbortController();
      const reason = new Error("caller cancelled");
      const pending = sandbox
        .exec({ command: ["x"], signal: controller.signal })
        .wait()
        .catch((e) => e);
      await h.clock.advance(1_000);
      expect(h.api.callsOf("GetIceServers")).toHaveLength(1);
      controller.abort(reason);
      const error = await h.clock.run(pending);
      expect(error).toBe(reason);
      expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
      expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
      expect(h.api.callsOf("OpenSession")).toHaveLength(0);
      expect(rtc.connections).toHaveLength(0);
    });
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

  for (const [name, message, expected] of [
    [
      "a missing file keeps the guest message",
      "read: file does not exist",
      "read: file does not exist",
    ],
    ["an expired read_id", "read expired", "the file read expired"],
  ] as const) {
    test(`a file read error frame with not_found: ${name}`, async () => {
      const { h } = p2pSetup((frame, peer) => {
        peer.send({
          streamId: frame.streamId,
          sandboxId: SANDBOX,
          kind: DataFrameKind.ERROR,
          payload: {
            case: "error",
            value: { code: ErrorCode.NOT_FOUND, message },
          },
        });
      });
      const sandbox = h.client.sandbox(SANDBOX);
      const error = await h.clock
        .run(sandbox.readFileBytes("/missing"))
        .catch((e) => e);
      expect(error).toBeInstanceOf(DataPathError);
      expect(error.code).toBe(Code.NotFound);
      expect(error.message).toBe(expected);
      expect(error.cause).toBeInstanceOf(ConnectError);
      expect(error.cause.rawMessage).toBe(message);
      expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
    });
  }

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

    test("a session-end frame on stream 0 does not end the session", async () => {
      let requests = 0;
      const { h, rtc } = p2pSetup((frame, peer) => {
        if (frame.kind === DataFrameKind.EXEC_REQUEST) {
          requests++;
          execEvent(peer, frame.streamId, ev.started());
          // SESSION_END on stream 0 fails ValidateFrame, so it is handled like
          // any other invalid frame: it is ignored and the session stays open.
          peer.send({
            streamId: 0n,
            kind: DataFrameKind.SESSION_END,
            payload: {
              case: "sessionEnd",
              value: { reason: SessionEndReason.EXPIRED },
            },
          });
          execEvent(peer, frame.streamId, ev.exited(0));
        }
      });
      const sandbox = h.client.sandbox(SANDBOX);
      const first = await h.clock.run(sandbox.run({ command: ["x"] }));
      expect(first.result.kind).toBe(ExecResultKind.Exited);
      expect(h.api.callsOf("OpenSession")).toHaveLength(1);
      // A following run reuses the session.
      const second = await h.clock.run(sandbox.run({ command: ["y"] }));
      expect(second.result.kind).toBe(ExecResultKind.Exited);
      expect(requests).toBe(2);
      expect(h.api.callsOf("OpenSession")).toHaveLength(1);
      expect(rtc.connections).toHaveLength(1);
    });

    test("a session-end frame without its payload fails only its open stream", async () => {
      const records: LogRecord[] = [];
      let goodStream: bigint | undefined;
      const { h, rtc } = p2pSetup(
        (frame, peer) => {
          if (frame.payload.case !== "execRequest") return;
          const command = frame.payload.value.command[0];
          if (command === "good") {
            goodStream = frame.streamId;
            execEvent(peer, frame.streamId, ev.started());
            execEvent(peer, frame.streamId, ev.stdout(0, "ok"));
          } else {
            // A session-end frame with no payload or with a payload of another
            // kind, on an open non-zero stream, while the good stream is open.
            peer.send({
              streamId: frame.streamId,
              kind: DataFrameKind.SESSION_END,
              payload: { case: undefined },
            });
            execEvent(peer, goodStream!, ev.exited(0));
          }
        },
        { logger: (record) => records.push(record) },
      );
      const sandbox = h.client.sandbox(SANDBOX);
      const good = sandbox.run({ command: ["good"] });
      await Promise.resolve();
      const bad = sandbox.run({ command: ["bad"] }).catch((e) => e);
      const [run, error] = await h.clock.run(Promise.all([good, bad]));
      expect(error).toBeInstanceOf(InvalidFrameError);
      expect(error).toBeInstanceOf(DataPathError);
      expect(text(run.stdout)).toBe("ok");
      expect(run.result.kind).toBe(ExecResultKind.Exited);
      expect(
        records.filter((r) => r.event === LogEvent.InvalidFrame),
      ).toHaveLength(1);
      expect(records.some((r) => r.event === LogEvent.SessionEnded)).toBe(
        false,
      );
      expect(h.api.callsOf("OpenSession")).toHaveLength(1);
      expect(rtc.connections).toHaveLength(1);
    });

    test("a session-end frame with another payload fails only its open stream", async () => {
      const { h } = p2pSetup((frame, peer) => {
        if (frame.kind === DataFrameKind.EXEC_REQUEST) {
          peer.send(execFrame(frame.streamId, DataFrameKind.SESSION_END));
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
      expect(h.api.callsOf("OpenSession")).toHaveLength(1);
    });

    test("an invalid session-end frame on a stream that is not open is ignored", async () => {
      const records: LogRecord[] = [];
      const { h, rtc } = p2pSetup(
        (frame, peer) => {
          if (frame.kind === DataFrameKind.EXEC_REQUEST) {
            peer.send(execFrame(77n, DataFrameKind.SESSION_END));
            execEvent(peer, frame.streamId, ev.started());
            execEvent(peer, frame.streamId, ev.exited(0));
          }
        },
        { logger: (record) => records.push(record) },
      );
      const sandbox = h.client.sandbox(SANDBOX);
      const first = await h.clock.run(sandbox.run({ command: ["a"] }));
      expect(first.result.kind).toBe(ExecResultKind.Exited);
      const second = await h.clock.run(sandbox.run({ command: ["b"] }));
      expect(second.result.kind).toBe(ExecResultKind.Exited);
      expect(
        records.filter((r) => r.event === LogEvent.InvalidFrame),
      ).toHaveLength(0);
      expect(h.api.callsOf("OpenSession")).toHaveLength(1);
      expect(rtc.connections).toHaveLength(1);
    });

    test("a valid session-end frame on a non-zero stream still ends the session", async () => {
      const records: LogRecord[] = [];
      let requests = 0;
      const { h } = p2pSetup(
        (frame, peer) => {
          if (frame.kind === DataFrameKind.EXEC_REQUEST) {
            requests++;
            execEvent(peer, frame.streamId, ev.started());
            peer.send({
              streamId: frame.streamId,
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
        },
        { logger: (record) => records.push(record) },
      );
      const run = await h.clock.run(
        h.client.sandbox(SANDBOX).run({ command: ["x"] }),
      );
      expect(run.result.kind).toBe(ExecResultKind.Exited);
      expect(requests).toBe(1);
      expect(h.api.callsOf("OpenSession")).toHaveLength(2);
      const ended = records.filter((r) => r.event === LogEvent.SessionEnded);
      expect(ended).toHaveLength(1);
      expect(ended[0]!.reason).toBe("EXPIRED");
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

    test("a read cancelled while the consumer is paused delivers no buffered chunk and no success", async () => {
      // Three chunks and the end frame arrive while the consumer holds the
      // first chunk: the queue then holds a buffered terminal frame.
      const file = source(3 << 10);
      const p = p2pSetup(readAgent(file, 1 << 10));
      const controller = new AbortController();
      const reason = new Error("caller cancelled");
      const chunks: Uint8Array[] = [];
      const wait = gate();
      const done = (async () => {
        for await (const chunk of p.h.client
          .sandbox(SANDBOX)
          .readFile("/big", { signal: controller.signal })) {
          chunks.push(chunk);
          if (chunks.length === 1) await wait.promise;
        }
        return "completed";
      })().catch((e: unknown) => e);
      await p.h.clock.advance(1000);
      expect(chunks).toHaveLength(1);
      controller.abort(reason);
      wait.open();
      expect(await p.h.clock.run(done)).toBe(reason);
      // Only the chunk that was delivered before the abort.
      expect(chunks).toHaveLength(1);
    });

    test("a transport failure still lets the consumer drain the buffered chunks and continues the read on the relay", async () => {
      const file4k = source(4 << 10);
      const kib = 1 << 10;
      let peerRef: FakeAgentPeer | undefined;
      // The agent sends the first three KiB and then never ends the read.
      const p = p2pSetup((frame, peer) => {
        if (frame.payload.case !== "fileReadRequest") return;
        peerRef = peer;
        for (let at = 0; at < 3 * kib; at += kib) {
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
                    data: file4k.subarray(at, at + kib),
                  },
                },
              },
            },
          });
        }
      });
      // The relay serves the rest of the file from the delivered offset.
      p.h.api.stream(RelayService, "ReadFile", function* () {
        yield file.accepted();
        yield file.chunk(3 * kib, file4k.subarray(3 * kib));
        yield file.end(4 * kib);
      });
      const chunks: Uint8Array[] = [];
      const wait = gate();
      // No catch: a rejected continuation must fail the test.
      const done = (async () => {
        for await (const chunk of p.h.client
          .sandbox(SANDBOX)
          .readFile("/big")) {
          chunks.push(chunk);
          if (chunks.length === 1) await wait.promise;
        }
      })();
      await p.h.clock.advance(1000);
      expect(chunks).toHaveLength(1);
      // The channel fails while two chunks are still buffered.
      peerRef!.fail();
      await p.h.clock.settle();
      wait.open();
      await p.h.clock.run(done);
      // The buffered chunks were delivered before the failure took effect;
      // the read then continued from the delivered offset.
      expect(Buffer.compare(concat(chunks), file4k)).toBe(0);
      expect(readOffsets(p.rtc)).toEqual([0n]);
      const peerRead = p.rtc.frames.find(
        (f) => f.payload.case === "fileReadRequest",
      )!.payload.value as { readId: string; path: string };
      const relayReads = p.h.api.callsOf("ReadFile");
      expect(relayReads).toHaveLength(1);
      const relayRead = relayReads[0]!.request as any;
      expect(relayRead.offset).toBe(BigInt(3 * kib));
      expect(relayRead.path).toBe("/big");
      expect(relayRead.path).toBe(peerRead.path);
      expect(relayRead.readId).toBe(peerRead.readId);
    });

    describe("a channel failure that an old session reports late", () => {
      /**
       * Three chunks of a read, no end frame, and an exec that starts and
       * exits at once; `execStarted` leaves an exec running instead.
       */
      function stallingAgent(execStarted = false): FakeAgent {
        const file = source(3 << 10);
        return (frame, peer) => {
          if (frame.payload.case === "execRequest") {
            execEvent(peer, frame.streamId, ev.started());
            if (!execStarted) {
              execEvent(peer, frame.streamId, ev.exited(0));
            }
            return;
          }
          if (frame.payload.case !== "fileReadRequest") return;
          for (let at = 0; at < file.length; at += 1 << 10) {
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
                      data: file.subarray(at, at + (1 << 10)),
                    },
                  },
                },
              },
            });
          }
        };
      }

      /**
       * Starts a read that pauses after its first chunk. The old channel then
       * fails while two chunks are buffered, so the read learns about the
       * failure only after its consumer resumes.
       */
      async function pausedReadThatLosesItsChannel(p: FakeWebRtcHarness) {
        const sandbox = p.h.client.sandbox(SANDBOX);
        const wait = gate();
        let chunks = 0;
        const done = (async () => {
          for await (const _ of sandbox.readFile("/big")) {
            if (++chunks === 1) await wait.promise;
          }
          return "completed";
        })().catch((e: unknown) => e);
        await p.h.clock.advance(1000);
        expect(chunks).toBe(1);
        expect(p.rtc.channels).toHaveLength(1);
        p.rtc.peers[0]!.fail();
        await p.h.clock.settle();
        return { sandbox, wait, done };
      }

      test("a forced peer-to-peer handle returns the error and keeps the healthy replacement", async () => {
        const p = p2pSetup(stallingAgent(), {
          transport: DataPathTransport.PEER_TO_PEER,
        });
        const { h, rtc } = p;
        const { sandbox, wait, done } = await pausedReadThatLosesItsChannel(p);
        // Another operation opens the replacement session while the read of
        // the old session still waits for its consumer.
        await h.clock.run(sandbox.run({ command: ["a"] }));
        expect(rtc.channels).toHaveLength(2);
        wait.open();
        const error = await h.clock.run(done);
        expect(error).toBeInstanceOf(DataPathError);
        expect((error as DataPathError).code).toBe(Code.Unavailable);
        expect((error as DataPathError).cause).toBeInstanceOf(
          PeerUnavailableError,
        );
        // The late failure did not retire the replacement.
        expect(rtc.channels[1]!.readyState).toBe("open");
        await h.clock.run(sandbox.run({ command: ["b"] }));
        expect(rtc.channels).toHaveLength(2);
        expect(h.api.callsOf("OpenSession")).toHaveLength(2);
        expect(h.api.callsOf("GetIceServers")).toHaveLength(2);
        expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
        expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
      });

      test("a failure of the installed session still retires it and a later operation reconnects", async () => {
        const p = p2pSetup(stallingAgent(), {
          transport: DataPathTransport.PEER_TO_PEER,
        });
        const { h, rtc } = p;
        const { sandbox, wait, done } = await pausedReadThatLosesItsChannel(p);
        // No other operation replaced the session: the failed one is still
        // the installed session when the read reports the failure.
        wait.open();
        const error = await h.clock.run(done);
        expect(error).toBeInstanceOf(DataPathError);
        expect((error as DataPathError).code).toBe(Code.Unavailable);
        expect(rtc.channels).toHaveLength(1);
        await h.clock.run(sandbox.run({ command: ["b"] }));
        expect(rtc.channels).toHaveLength(2);
        expect(h.api.callsOf("OpenSession")).toHaveLength(2);
        expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
        expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
      });

      test("an automatic handle still falls back to the relay and does not close the replacement session", async () => {
        const p = p2pSetup(stallingAgent());
        const { h, rtc } = p;
        h.api.stream(RelayService, "ReadFile", function* () {
          yield file.accepted();
          yield file.chunk(0, "relay");
          yield file.end(5);
        });
        const { sandbox, wait, done } = await pausedReadThatLosesItsChannel(p);
        await h.clock.run(sandbox.run({ command: ["a"] }));
        expect(rtc.channels).toHaveLength(2);
        wait.open();
        // The read continues on the relay from its delivered offset.
        await h.clock.run(done);
        expect(sandbox.transport).toBe(DataPathTransport.RELAY);
        expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(1);
        // The stale failure does not close the session of the other operation.
        expect(rtc.channels[1]!.readyState).toBe("open");
      });

      test("an exec of an old session that fails late keeps the healthy replacement", async () => {
        const p = p2pSetup(stallingAgent(true), {
          transport: DataPathTransport.PEER_TO_PEER,
        });
        const { h, rtc } = p;
        const sandbox = h.client.sandbox(SANDBOX);
        const wait = gate();
        let events = 0;
        const done = (async () => {
          for await (const _ of sandbox.exec({ command: ["x"] })) {
            if (++events === 1) await wait.promise;
          }
          return "completed";
        })().catch((e: unknown) => e);
        await h.clock.advance(1000);
        expect(events).toBe(1);
        rtc.peers[0]!.fail();
        await h.clock.settle();
        // A read opens the replacement session.
        const read = sandbox.readFileBytes("/big").catch((e: unknown) => e);
        await h.clock.advance(1000);
        expect(rtc.channels).toHaveLength(2);
        wait.open();
        const error = await h.clock.run(done);
        expect(error).toBeInstanceOf(DataPathError);
        expect(rtc.channels[1]!.readyState).toBe("open");
        expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
        // Settle the read that waits for its end frame.
        rtc.peers[1]!.fail();
        await h.clock.run(read);
      });
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

    test("a caller aborted while its frame waits for the send turn returns before the turn is released", async () => {
      const p = p2pSetup(writeAgent());
      const { h, rtc } = p;
      const { sandbox, channel } = await warmUp(p);
      channel.bufferedAmount = FULL;
      let messages = 0;
      channel.afterSend = () => messages++;
      // The first write holds the send turn while it waits for the buffer.
      const holder = sandbox.writeFile("/a", new Uint8Array(40 << 10).fill(7));
      await h.clock.settle();
      const controller = new AbortController();
      const reason = new Error("caller cancelled");
      let outcome: unknown = "pending";
      const cancelled = sandbox
        .run({ command: ["rm", "-rf", "/data"], signal: controller.signal })
        .then(
          () => (outcome = "completed"),
          (error: unknown) => (outcome = error),
        );
      // A later request queues behind the cancelled one.
      const later = sandbox.writeFile("/c", new Uint8Array(40 << 10).fill(9));
      await h.clock.settle();
      expect(outcome).toBe("pending");
      controller.abort(reason);
      await h.clock.settle();
      // The caller is released with its reason although the buffer is still
      // full and the active sender still holds the turn.
      expect(outcome).toBe(reason);
      expect(messages).toBe(0);
      await cancelled;
      // The active sender and the session are still usable; the later frame
      // keeps its place after the active one.
      for (let i = 0; i < 10 && messages < 6; i++) {
        channel.drain(0);
        await h.clock.settle();
      }
      const [heldWrite, laterWrite] = await h.clock.run(
        Promise.all([holder, later]),
      );
      expect(heldWrite.sizeBytes).toBe(BigInt(40 << 10));
      expect(laterWrite.sizeBytes).toBe(BigInt(40 << 10));
      expect(messages).toBe(6);
      expect(
        rtc.frames.map((frame) =>
          frame.payload.case === "fileWriteChunk"
            ? frame.payload.value.path
            : frame.kind,
        ),
      ).toEqual(["/warm", "/a", "/c"]);
      expect(rtc.channels).toHaveLength(1);
      expect(channel.readyState).toBe("open");
    });

    test("an abort during a frame in progress returns at once and still lets that frame finish", async () => {
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
      let outcome: unknown = "pending";
      const write = sandbox
        .writeFile("/a", new Uint8Array(40 << 10).fill(7), {
          signal: controller.signal,
        })
        .then(
          () => (outcome = "completed"),
          (error: unknown) => (outcome = error),
        );
      await h.clock.settle();
      channel.drain();
      await h.clock.settle();
      expect(messages).toBe(1);
      controller.abort(reason);
      await h.clock.settle();
      expect(outcome).toBe(reason);
      for (let i = 0; i < 10 && messages < 3; i++) {
        channel.drain();
        await h.clock.settle();
      }
      expect(messages).toBe(3);
      expect(rtc.frames).toHaveLength(2);
      await write;
    });

    test("a send that throws in the middle of a frame retires the session and no later frame follows", async () => {
      const p = p2pSetup(writeAgent());
      const { h, rtc } = p;
      const { sandbox, channel } = await warmUp(p);
      let messages = 0;
      channel.afterSend = () => {
        messages++;
        if (messages === 2) {
          // The channel took the first message of the frame and fails on the
          // second, as an RTCDataChannel does with an OperationError.
          throw new Error("OperationError: send queue is full");
        }
      };
      const partial = sandbox
        .writeFile("/a", new Uint8Array(40 << 10).fill(7))
        .catch((e) => e);
      const queued = sandbox
        .writeFile("/b", new Uint8Array(40 << 10).fill(8))
        .catch((e) => e);
      await h.clock.settle();
      // Only two messages of the first frame were written; the queued frame
      // never followed them on the dead byte stream.
      expect(messages).toBe(2);
      expect(channel.readyState).toBe("closed");
      await h.clock.run(Promise.all([partial, queued]));
      expect(messages).toBe(2);
      expect(rtc.frames).toHaveLength(1);
    });

    describe("a first message that fails on a channel that is not open", () => {
      function installRelayRead(h: Harness) {
        h.api.stream(RelayService, "ReadFile", function* () {
          yield file.accepted();
          yield file.chunk(0, "relay");
          yield file.end(5);
        });
      }

      for (const readyState of ["closing", "closed"]) {
        test(`a ${readyState} channel moves an automatic handle to the relay before its close event`, async () => {
          const p = p2pSetup(writeAgent());
          const { h, rtc } = p;
          const { sandbox, channel } = await warmUp(p);
          installRelayRead(h);
          // The close event of the channel has not arrived: the session is
          // still installed, but every send fails.
          channel.readyState = readyState;
          const bytes = await h.clock.run(sandbox.readFileBytes("/a"));
          expect(text(bytes)).toBe("relay");
          expect(sandbox.transport).toBe(DataPathTransport.RELAY);
          expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(1);
          const reads = h.api.callsOf("ReadFile");
          expect(reads).toHaveLength(1);
          expect(
            (reads[0]!.request as unknown as { offset: bigint }).offset,
          ).toBe(0n);
          // No second peer was opened, and the next operation uses the relay.
          expect(rtc.channels).toHaveLength(1);
          expect(h.api.callsOf("OpenSession")).toHaveLength(1);
          await h.clock.run(sandbox.readFileBytes("/a"));
          expect(h.api.callsOf("ReadFile")).toHaveLength(2);
          expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(2);
          expect(h.api.callsOf("OpenSession")).toHaveLength(1);
          expect(rtc.channels).toHaveLength(1);
        });
      }

      test("a forced peer-to-peer handle returns the unavailable error and no relay authorization", async () => {
        const p = p2pSetup(writeAgent(), {
          transport: DataPathTransport.PEER_TO_PEER,
        });
        const { h } = p;
        const { sandbox, channel } = await warmUp(p);
        installRelayRead(h);
        channel.readyState = "closing";
        const error = await h.clock
          .run(sandbox.readFileBytes("/a"))
          .catch((e) => e);
        expect(error).toBeInstanceOf(DataPathError);
        expect((error as DataPathError).code).toBe(Code.Unavailable);
        expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
        expect(h.api.callsOf("ReadFile")).toHaveLength(0);
        expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
      });

      test("the send failure is the cause of the peer failure", async () => {
        const p = p2pSetup(writeAgent(), {
          transport: DataPathTransport.PEER_TO_PEER,
        });
        const { h } = p;
        const { sandbox, channel } = await warmUp(p);
        channel.readyState = "closing";
        const error = await h.clock
          .run(sandbox.readFileBytes("/a"))
          .catch((e) => e);
        const peerFailure = (error as DataPathError).cause;
        expect(peerFailure).toBeInstanceOf(PeerUnavailableError);
        expect(((peerFailure as Error).cause as Error).message).toBe(
          "channel not open",
        );
      });

      test("a first message that fails on a channel that is still open keeps the session", async () => {
        const p = p2pSetup(writeAgent());
        const { h, rtc } = p;
        const { sandbox, channel } = await warmUp(p);
        channel.sendError = new Error("OperationError: send queue is full");
        const error = await h.clock
          .run(sandbox.run({ command: ["a"] }))
          .catch((e) => e);
        expect(error).toBeInstanceOf(Error);
        expect((error as Error).message).toContain("send queue is full");
        channel.sendError = undefined;
        // The session is not retired: the next write uses it.
        await h.clock.run(sandbox.writeFile("/b", "x"));
        expect(rtc.channels).toHaveLength(1);
        expect(h.api.callsOf("OpenSession")).toHaveLength(1);
        expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
        expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
      });
    });

    test("a caller aborted while its first message waits for the buffer sends nothing", async () => {
      const p = p2pSetup(writeAgent());
      const { h, rtc } = p;
      const { sandbox, channel } = await warmUp(p);
      channel.bufferedAmount = FULL;
      let messages = 0;
      channel.afterSend = () => messages++;
      const controller = new AbortController();
      const reason = new Error("caller cancelled");
      const cancelled = sandbox
        .run({ command: ["rm", "-rf", "/data"], signal: controller.signal })
        .catch((e) => e);
      await h.clock.settle();
      expect(messages).toBe(0);
      controller.abort(reason);
      await h.clock.settle();
      channel.drain(0);
      await h.clock.settle();
      expect(await h.clock.run(cancelled)).toBe(reason);
      expect(messages).toBe(0);
      expect(
        rtc.frames.some((frame) => frame.kind === DataFrameKind.EXEC_REQUEST),
      ).toBe(false);
      // The send turn is released, so a later request still goes out.
      const written = await h.clock.run(
        sandbox.writeFile("/a", new Uint8Array(40 << 10).fill(7)),
      );
      expect(written.sizeBytes).toBe(BigInt(40 << 10));
      expect(messages).toBe(3);
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

    test("a session end releases the backpressured sender and the queued senders even if the old channel emits no event", async () => {
      const p = p2pSetup(writeAgent());
      const { h, rtc } = p;
      const { sandbox, channel } = await warmUp(p);
      // The channel that the session closes emits neither `close` nor
      // `bufferedamountlow`, as a channel of a closed peer connection can.
      channel.suppressCloseEvent = true;
      let messages = 0;
      channel.afterSend = () => {
        messages++;
        // The buffer stays full after each fragment, so the sender must wait.
        channel.bufferedAmount = FULL;
      };
      channel.bufferedAmount = FULL;
      const waiting = sandbox
        .writeFile("/a", new Uint8Array(40 << 10))
        .catch((e) => e);
      const queued = sandbox
        .writeFile("/b", new Uint8Array(40 << 10))
        .catch((e) => e);
      await h.clock.settle();
      // One fragment of the first frame goes out when the buffer drains once.
      channel.drain();
      await h.clock.settle();
      expect(messages).toBe(1);
      rtc.peers[0]!.endSession(SessionEndReason.UNSPECIFIED);
      // No timer is pending and the channel never signals again: only the
      // session end can release the senders.
      const results = await h.clock.run(Promise.all([waiting, queued]));
      for (const result of results) {
        expect(result).not.toBeInstanceOf(Error);
      }
      // The old channel sent no further fragment after the end; both writes
      // completed on one replacement session.
      expect(messages).toBe(1);
      expect(channel.readyState).toBe("closed");
      expect(rtc.channels).toHaveLength(2);
      expect(h.api.callsOf("OpenSession")).toHaveLength(2);
      expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
    });

    test("closing the handle releases a backpressured sender even if the channel emits no event", async () => {
      const p = p2pSetup(writeAgent());
      const { h } = p;
      const { sandbox, channel } = await warmUp(p);
      channel.suppressCloseEvent = true;
      channel.bufferedAmount = FULL;
      const waiting = sandbox
        .writeFile("/a", new Uint8Array(40 << 10))
        .catch((e) => e);
      const queued = sandbox
        .writeFile("/b", new Uint8Array(40 << 10))
        .catch((e) => e);
      await h.clock.settle();
      sandbox.close();
      const results = await h.clock.run(Promise.all([waiting, queued]));
      for (const result of results) {
        expect(result).toBeInstanceOf(OwnSandHandleClosedError);
      }
    });
  });

  describe("closing the sandbox handle", () => {
    const exiting: FakeAgent = (frame, peer) => {
      if (frame.kind === DataFrameKind.EXEC_REQUEST) {
        execEvent(peer, frame.streamId, ev.started());
        execEvent(peer, frame.streamId, ev.exited(0));
      }
    };

    test("a close while the shared setup is pending ends every waiter and sends no OpenSession", async () => {
      const { h, rtc } = p2pSetup(exiting);
      const sandbox = h.client.sandbox(SANDBOX);
      const controller = new AbortController();
      const withSignal = sandbox
        .run({ command: ["a"], signal: controller.signal })
        .catch((e) => e);
      const withoutSignal = sandbox.run({ command: ["b"] }).catch((e) => e);
      // Setup is pending (ICE gathering takes virtual time).
      await h.clock.advance(50);
      sandbox.close();
      const errors = await h.clock.run(
        Promise.all([withSignal, withoutSignal]),
      );
      expect(errors[0]).toBeInstanceOf(OwnSandHandleClosedError);
      expect(errors[1]).toBeInstanceOf(OwnSandHandleClosedError);
      expect((errors[0] as Error).message).toBe("sandbox handle closed");
      // The setup was cancelled before the offer: no session was created.
      expect(h.api.callsOf("OpenSession")).toHaveLength(0);
      expect(rtc.channels[0]!.readyState).toBe("closed");
      // The close is not a peer failure: the handle stays on peer-to-peer.
      expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
      expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
    });

    describe.each([
      // The method that the WebRTC stack never answers, and the OpenSession
      // calls that the setup has made when it stops there.
      ["createOffer", 0],
      ["setLocalDescription", 0],
      ["setRemoteDescription", 1],
    ] as const)(
      "while %s of the peer connection is pending",
      (method, opens) => {
        /** Starts two waiters, one without a signal, and holds `method`. */
        async function start() {
          const { h, rtc } = p2pSetup(exiting, { hold: [method] });
          const sandbox = h.client.sandbox(SANDBOX);
          const controller = new AbortController();
          const withSignal = sandbox
            .run({ command: ["a"], signal: controller.signal })
            .catch((e) => e);
          const withoutSignal = sandbox.run({ command: ["b"] }).catch((e) => e);
          // Gathering takes 100 virtual ms and the fake OpenSession is instant.
          await h.clock.advance(1_000);
          expect(rtc.held(method)).toBe(1);
          expect(rtc.connections).toHaveLength(1);
          expect(h.api.callsOf("OpenSession")).toHaveLength(opens);
          return { h, rtc, sandbox, controller, withSignal, withoutSignal };
        }

        test("a close ends every waiter, closes the peer connection and starts no relay", async () => {
          const { h, rtc, sandbox, withSignal, withoutSignal } = await start();
          sandbox.close();
          const errors = await h.clock.run(
            Promise.all([withSignal, withoutSignal]),
          );
          expect(errors[0]).toBeInstanceOf(OwnSandHandleClosedError);
          expect(errors[1]).toBeInstanceOf(OwnSandHandleClosedError);
          expect(rtc.closedConnections).toBe(1);
          expect(rtc.channels[0]!.readyState).toBe("closed");
          expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
          expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
        });

        test("a late completion after the close installs no peer", async () => {
          const { h, rtc, sandbox, withSignal, withoutSignal } = await start();
          sandbox.close();
          await h.clock.run(Promise.all([withSignal, withoutSignal]));
          rtc.release(method);
          await h.clock.settle();
          // The late completion starts no further signaling or channel work.
          expect(h.api.callsOf("OpenSession")).toHaveLength(opens);
          expect(rtc.connections).toHaveLength(1);
          expect(rtc.frames).toHaveLength(0);
          expect(rtc.channels[0]!.readyState).toBe("closed");
          await expect(
            h.clock.run(sandbox.run({ command: ["c"] })),
          ).rejects.toBeInstanceOf(OwnSandHandleClosedError);
          expect(rtc.connections).toHaveLength(1);
          expect(h.api.callsOf("GetIceServers")).toHaveLength(1);
          expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
        });

        test("the last caller that aborts ends with its reason and closes the peer connection", async () => {
          const { h, rtc, sandbox, controller, withSignal, withoutSignal } =
            await start();
          const reason = new Error("caller cancelled");
          // The caller without a signal still waits, so the abort keeps the
          // shared setup.
          controller.abort(reason);
          expect(await h.clock.run(withSignal)).toBe(reason);
          expect(rtc.closedConnections).toBe(0);
          // The close then ends the remaining waiter.
          sandbox.close();
          expect(await h.clock.run(withoutSignal)).toBeInstanceOf(
            OwnSandHandleClosedError,
          );
          expect(rtc.closedConnections).toBe(1);
        });
      },
    );

    test("a caller abort while the offer is pending cancels the setup and the next caller starts afresh", async () => {
      const { h, rtc } = p2pSetup(exiting, { hold: ["createOffer"] });
      const sandbox = h.client.sandbox(SANDBOX);
      const controller = new AbortController();
      const reason = new Error("caller cancelled");
      const first = sandbox
        .run({ command: ["a"], signal: controller.signal })
        .catch((e) => e);
      await h.clock.advance(1_000);
      expect(rtc.held("createOffer")).toBe(1);
      controller.abort(reason);
      expect(await h.clock.run(first)).toBe(reason);
      // The sole waiter left: the setup was cancelled and its peer closed.
      expect(rtc.closedConnections).toBe(1);
      expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
      expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
    });

    test("a session that opens after the close is closed and never installed", async () => {
      const { h, rtc } = p2pSetup(exiting, {
        openSessionMs: 5_000,
        ignoreOpenSessionAbort: true,
      });
      const sandbox = h.client.sandbox(SANDBOX);
      const pending = sandbox.run({ command: ["a"] }).catch((e) => e);
      // 100 ms of gathering, then OpenSession is in flight and still waits for
      // its scripted answer.
      await h.clock.advance(1_000);
      expect(h.api.callsOf("OpenSession")).toHaveLength(1);
      expect(rtc.connections).toHaveLength(1);
      sandbox.close();
      const error = await h.clock.run(pending);
      // The successful answer reached the SDK only after the close (the call
      // runs to its scripted end because the abort signal is hidden from it).
      expect(h.clock.now()).toBeGreaterThanOrEqual(5_000);
      expect(error).toBeInstanceOf(OwnSandHandleClosedError);
      expect(rtc.channels[0]!.readyState).toBe("closed");
      expect(rtc.frames).toHaveLength(0);
      expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
      expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
      // A later operation is rejected before any new setup.
      await expect(
        h.clock.run(sandbox.run({ command: ["b"] })),
      ).rejects.toBeInstanceOf(OwnSandHandleClosedError);
      expect(rtc.connections).toHaveLength(1);
      expect(h.api.callsOf("GetIceServers")).toHaveLength(1);
      expect(h.api.callsOf("OpenSession")).toHaveLength(1);
      expect(rtc.frames).toHaveLength(0);
    });

    test("a Canceled error from OpenSession after the close still ends with the closed error", async () => {
      const { h, rtc } = p2pSetup(exiting, { ignoreOpenSessionAbort: true });
      let delivered = 0;
      h.api.unary(DataPathService, "OpenSession", async () => {
        await h.clock.sleep(5_000);
        delivered++;
        throw new ConnectError("canceled", Code.Canceled);
      });
      const sandbox = h.client.sandbox(SANDBOX);
      const pending = sandbox.run({ command: ["a"] }).catch((e) => e);
      await h.clock.advance(1_000);
      // OpenSession is in flight and has not delivered its outcome yet.
      expect(h.api.callsOf("OpenSession")).toHaveLength(1);
      expect(delivered).toBe(0);
      sandbox.close();
      expect(delivered).toBe(0);
      expect(await h.clock.run(pending)).toBeInstanceOf(
        OwnSandHandleClosedError,
      );
      // The explicit Canceled outcome reached the SDK after the close.
      expect(delivered).toBe(1);
      expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
      expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
      expect(rtc.channels[0]!.readyState).toBe("closed");
      expect(rtc.frames).toHaveLength(0);
      await expect(
        h.clock.run(sandbox.run({ command: ["b"] })),
      ).rejects.toBeInstanceOf(OwnSandHandleClosedError);
      expect(h.api.callsOf("GetIceServers")).toHaveLength(1);
      expect(h.api.callsOf("OpenSession")).toHaveLength(1);
    });

    describe("from the access token callback of the first request", () => {
      const TOKEN = "osk_test_0123456789abcdef";

      test("a synchronous abort ends with the exact reason even if the token never resolves", async () => {
        const reason = new Error("caller cancelled");
        const controller = new AbortController();
        let release!: (token: string) => void;
        const { h, rtc } = p2pSetup(exiting, {
          accessToken: () => {
            controller.abort(reason);
            return new Promise<string>((resolve) => (release = resolve));
          },
        });
        const sandbox = h.client.sandbox(SANDBOX);
        const pending = sandbox
          .run({ command: ["a"], signal: controller.signal })
          .catch((e) => e);
        // The abort is prompt: no timer and no token is needed.
        expect(await h.clock.run(pending)).toBe(reason);
        // A token that arrives after the abort starts no RPC.
        release(TOKEN);
        await h.clock.settle();
        expect(h.api.callsOf("GetIceServers")).toHaveLength(0);
        expect(h.api.callsOf("OpenSession")).toHaveLength(0);
        expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
        expect(rtc.connections).toHaveLength(0);
        expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
      });

      test("the last waiter that aborts cancels the setup and a later caller starts a fresh one", async () => {
        const controller = new AbortController();
        let calls = 0;
        const { h } = p2pSetup(exiting, {
          accessToken: () => {
            if (calls++ === 0) {
              controller.abort(new Error("first caller cancelled"));
              return new Promise<string>(() => {});
            }
            return TOKEN;
          },
        });
        const sandbox = h.client.sandbox(SANDBOX);
        await h.clock.run(
          sandbox
            .run({ command: ["a"], signal: controller.signal })
            .catch((e) => e),
        );
        expect(h.api.callsOf("GetIceServers")).toHaveLength(0);
        const run = await h.clock.run(sandbox.run({ command: ["b"] }));
        expect(run.result.kind).toBe(ExecResultKind.Exited);
        expect(h.api.callsOf("GetIceServers")).toHaveLength(1);
        expect(h.api.callsOf("OpenSession")).toHaveLength(1);
        expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
      });

      test("an abort of one of two callers that share the setup leaves the other served", async () => {
        const controller = new AbortController();
        const reason = new Error("first caller cancelled");
        let release!: (token: string) => void;
        let calls = 0;
        const { h } = p2pSetup(exiting, {
          // Only the first request waits for its token; later requests of the
          // shared setup get one at once.
          accessToken: () =>
            calls++ === 0 ? new Promise<string>((r) => (release = r)) : TOKEN,
        });
        const sandbox = h.client.sandbox(SANDBOX);
        const first = sandbox
          .run({ command: ["a"], signal: controller.signal })
          .catch((e) => e);
        const second = sandbox.run({ command: ["b"] });
        await h.clock.settle();
        controller.abort(reason);
        expect(await h.clock.run(first)).toBe(reason);
        release(TOKEN);
        const run = await h.clock.run(second);
        expect(run.result.kind).toBe(ExecResultKind.Exited);
        expect(h.api.callsOf("GetIceServers")).toHaveLength(1);
        expect(h.api.callsOf("OpenSession")).toHaveLength(1);
      });

      test("a synchronous close aborts the setup and ends with the closed error", async () => {
        let sandbox!: Sandbox;
        let release!: (token: string) => void;
        let calls = 0;
        const { h, rtc } = p2pSetup(exiting, {
          accessToken: () => {
            calls++;
            sandbox.close();
            return new Promise<string>((resolve) => (release = resolve));
          },
        });
        sandbox = h.client.sandbox(SANDBOX);
        const withSignal = sandbox
          .run({ command: ["a"], signal: new AbortController().signal })
          .catch((e) => e);
        const error = await h.clock.run(withSignal);
        expect(error).toBeInstanceOf(OwnSandHandleClosedError);
        // A late token starts no RPC and no relay fallback follows.
        release(TOKEN);
        await h.clock.settle();
        expect(calls).toBe(1);
        expect(h.api.callsOf("GetIceServers")).toHaveLength(0);
        expect(h.api.callsOf("OpenSession")).toHaveLength(0);
        expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
        expect(rtc.connections).toHaveLength(0);
        expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
        // A later peer operation rejects before GetIceServers.
        await expect(
          h.clock.run(sandbox.run({ command: ["b"] })),
        ).rejects.toBeInstanceOf(OwnSandHandleClosedError);
        expect(calls).toBe(1);
        expect(h.api.callsOf("GetIceServers")).toHaveLength(0);
      });

      test("a synchronous close ends a caller without a signal too", async () => {
        let sandbox!: Sandbox;
        const { h } = p2pSetup(exiting, {
          accessToken: () => {
            sandbox.close();
            return new Promise<string>(() => {});
          },
        });
        sandbox = h.client.sandbox(SANDBOX);
        await expect(
          h.clock.run(sandbox.run({ command: ["a"] })),
        ).rejects.toBeInstanceOf(OwnSandHandleClosedError);
        expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
      });
    });

    test("close is idempotent and a closed handle rejects before GetIceServers", async () => {
      const { h, rtc } = p2pSetup(exiting);
      const sandbox = h.client.sandbox(SANDBOX);
      // No session was ever opened.
      sandbox.close();
      sandbox.close();
      await expect(
        h.clock.run(sandbox.run({ command: ["a"] })),
      ).rejects.toBeInstanceOf(OwnSandHandleClosedError);
      expect(h.api.callsOf("GetIceServers")).toHaveLength(0);
      expect(rtc.connections).toHaveLength(0);
      expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
      sandbox.close();
    });

    test("a close after the session opened closes it and rejects later operations", async () => {
      const { h, rtc } = p2pSetup(exiting);
      const sandbox = h.client.sandbox(SANDBOX);
      await h.clock.run(sandbox.run({ command: ["a"] }));
      sandbox.close();
      sandbox.close();
      expect(rtc.channels[0]!.readyState).toBe("closed");
      await expect(
        h.clock.run(sandbox.run({ command: ["b"] })),
      ).rejects.toBeInstanceOf(OwnSandHandleClosedError);
      expect(rtc.connections).toHaveLength(1);
      expect(h.api.callsOf("OpenSession")).toHaveLength(1);
    });

    test("a closed relay-only handle keeps serving", async () => {
      for (const options of [
        { transport: DataPathTransport.RELAY },
        // A WebRTC-less client uses the relay too.
        undefined,
      ]) {
        const h =
          options === undefined ? setup() : p2pSetup(() => {}, options).h;
        h.api.stream(RelayService, "Exec", function* () {
          yield execMsg(ev.accepted());
          yield execMsg(ev.exited(0));
        });
        const sandbox = h.client.sandbox(SANDBOX);
        sandbox.close();
        const result = await h.clock.run(
          sandbox.exec({ command: ["x"] }).wait(),
        );
        expect(result.kind).toBe(ExecResultKind.Exited);
        expect(sandbox.transport).toBe(DataPathTransport.RELAY);
        expect(h.api.callsOf("OpenSession")).toHaveLength(0);
      }
    });

    describe("an operation in flight at the close", () => {
      /** Accepts every request and never finishes it. */
      const hanging: FakeAgent = (frame, peer) => {
        if (frame.kind === DataFrameKind.EXEC_REQUEST) {
          execEvent(peer, frame.streamId, ev.started());
        }
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
        }
        if (
          frame.payload.case === "fileWriteChunk" &&
          frame.payload.value.offset === 0n
        ) {
          // Answers the first chunk only: the close arrives between chunks.
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
                finished: false,
                sizeBytes: 0n,
              },
            },
          });
        }
      };

      const operations: Record<string, (sandbox: Sandbox) => Promise<unknown>> =
        {
          exec: (sandbox) => sandbox.exec({ command: ["x"] }).wait(),
          readFile: async (sandbox) => {
            for await (const chunk of sandbox.readFile("/a")) {
              void chunk;
            }
          },
          writeFile: (sandbox) =>
            // Two chunks: the agent answers the first and the close arrives
            // while the second is in flight.
            sandbox.writeFile("/a", new Uint8Array((1 << 20) + 1).fill(7)),
        };

      /** Fails the test when a later operation reaches the relay. */
      function expectNoRelay(h: ReturnType<typeof p2pSetup>["h"]) {
        expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(0);
        expect(h.api.callsOf("Exec")).toHaveLength(0);
        expect(h.api.callsOf("AttachExec")).toHaveLength(0);
        expect(h.api.callsOf("ReadFile")).toHaveLength(0);
        expect(h.api.callsOf("WriteFile")).toHaveLength(0);
      }

      async function expectLaterOperationsRejected(sandbox: Sandbox) {
        const { h } = current!;
        await expect(
          h.clock.run(sandbox.run({ command: ["y"] })),
        ).rejects.toBeInstanceOf(OwnSandHandleClosedError);
        await expect(
          h.clock.run(sandbox.exec({ command: ["y"] }).wait()),
        ).rejects.toBeInstanceOf(OwnSandHandleClosedError);
        await expect(
          h.clock.run(sandbox.readFileBytes("/b")),
        ).rejects.toBeInstanceOf(OwnSandHandleClosedError);
        await expect(
          h.clock.run(sandbox.writeFile("/b", "x")),
        ).rejects.toBeInstanceOf(OwnSandHandleClosedError);
        expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
        expect(h.api.callsOf("OpenSession")).toHaveLength(1);
        expectNoRelay(h);
      }

      let current: ReturnType<typeof p2pSetup> | undefined;

      for (const name of Object.keys(operations)) {
        for (const close of ["close", "delete"] as const) {
          test(`${name} ends with the closed error after ${close}, without a relay fall back`, async () => {
            current = p2pSetup(hanging);
            const { h, rtc } = current;
            h.api.unary(SandboxService, "DeleteSandbox", (req) => ({
              sandbox: { sandboxId: req.sandboxId },
            }));
            const sandbox = h.client.sandbox(SANDBOX);
            const pending = operations[name]!(sandbox).catch((e) => e);
            // The session is open and the request is on the channel.
            await h.clock.advance(1_000);
            expect(rtc.frames.length).toBeGreaterThan(0);
            if (close === "close") {
              sandbox.close();
            } else {
              await h.clock.run(sandbox.delete());
            }
            const error = await h.clock.run(pending);
            expect(error).toBeInstanceOf(OwnSandHandleClosedError);
            expect(sandbox.transport).toBe(DataPathTransport.PEER_TO_PEER);
            expectNoRelay(h);
            await expectLaterOperationsRejected(sandbox);
          });
        }
      }

      test("a forced PEER_TO_PEER handle ends with the closed error, not a data path error", async () => {
        current = p2pSetup(hanging, {
          transport: DataPathTransport.PEER_TO_PEER,
        });
        const { h, rtc } = current;
        const sandbox = h.client.sandbox(SANDBOX);
        const pending = sandbox
          .exec({ command: ["x"] })
          .wait()
          .catch((e) => e);
        await h.clock.advance(1_000);
        expect(rtc.frames).toHaveLength(1);
        sandbox.close();
        const error = await h.clock.run(pending);
        expect(error).toBeInstanceOf(OwnSandHandleClosedError);
        expect(error).not.toBeInstanceOf(DataPathError);
        await expectLaterOperationsRejected(sandbox);
      });

      test("a channel failure on an open handle still moves it to the relay", async () => {
        const { h, rtc } = p2pSetup((frame, peer) => {
          if (frame.kind === DataFrameKind.EXEC_REQUEST) {
            execEvent(peer, frame.streamId, ev.started());
            queueMicrotask(() => queueMicrotask(() => peer.fail()));
          }
        });
        h.api.stream(RelayService, "AttachExec", function* () {
          yield attachMsg(ev.accepted());
          yield attachMsg(ev.started());
          yield attachMsg(ev.exited(0));
        });
        h.api.stream(RelayService, "Exec", function* () {
          yield execMsg(ev.accepted());
          yield execMsg(ev.exited(0));
        });
        const sandbox = h.client.sandbox(SANDBOX);
        const result = await h.clock.run(
          sandbox.exec({ command: ["x"] }).wait(),
        );
        expect(result.kind).toBe(ExecResultKind.Exited);
        expect(sandbox.transport).toBe(DataPathTransport.RELAY);
        expect(rtc.channels[0]!.readyState).toBe("closed");
        expect(h.api.callsOf("AttachExec")).toHaveLength(1);
        // The relay handle keeps serving after a later close.
        sandbox.close();
        const again = await h.clock.run(
          sandbox.exec({ command: ["y"] }).wait(),
        );
        expect(again.kind).toBe(ExecResultKind.Exited);
      });
    });

    test("delete closes the handle only after DeleteSandbox succeeded", async () => {
      const { h, rtc } = p2pSetup(exiting);
      let fail = true;
      h.api.unary(SandboxService, "DeleteSandbox", (req) => {
        if (fail) {
          throw new ConnectError("busy", Code.Unavailable);
        }
        return { sandbox: { sandboxId: req.sandboxId } };
      });
      const sandbox = h.client.sandbox(SANDBOX);
      await h.clock.run(sandbox.run({ command: ["a"] }));
      await expect(h.clock.run(sandbox.delete())).rejects.toBeInstanceOf(
        ConnectError,
      );
      // The failed delete left the handle and its session usable.
      expect(rtc.channels[0]!.readyState).not.toBe("closed");
      await h.clock.run(sandbox.run({ command: ["b"] }));
      expect(h.api.callsOf("OpenSession")).toHaveLength(1);
      fail = false;
      await h.clock.run(sandbox.delete());
      expect(rtc.channels[0]!.readyState).toBe("closed");
      await expect(
        h.clock.run(sandbox.run({ command: ["c"] })),
      ).rejects.toBeInstanceOf(OwnSandHandleClosedError);
    });
  });
});
