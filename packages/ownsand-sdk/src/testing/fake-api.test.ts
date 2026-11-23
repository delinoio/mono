// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import { RelayService } from "@delino/ownsand-protos/v1/relay_pb";
import { SandboxService } from "@delino/ownsand-protos/v1/sandbox_pb";
import { Disconnect, Eof, Hang, Sleep } from "./fake-api.ts";
import { ev, execMsg, setup } from "./harness.ts";

const EXEC_URL = `https://ownsand.api.test/${RelayService.typeName}/Exec`;
// One enveloped, empty request message.
const REQUEST_BODY = new Uint8Array([0, 0, 0, 0, 0]);
const GET_SANDBOX_URL = `https://ownsand.api.test/${SandboxService.typeName}/GetSandbox`;

/** Calls GetSandbox with an empty request, a valid empty message. */
function getSandbox(h: ReturnType<typeof setup>, signal?: AbortSignal) {
  return h.api.fetch(GET_SANDBOX_URL, {
    method: "POST",
    headers: { "content-type": "application/proto" },
    body: new Uint8Array(0),
    signal,
  });
}

/** Starts the Exec stream with an optional signal. */
function openExec(h: ReturnType<typeof setup>, signal?: AbortSignal) {
  return h.api.fetch(EXEC_URL, {
    method: "POST",
    headers: { "content-type": "application/connect+proto" },
    body: REQUEST_BODY,
    signal,
  });
}

type Read = { at: number; flags: number | undefined };

/** Starts the Exec stream and returns a reader plus its abort controller. */
async function open(h: ReturnType<typeof setup>) {
  const controller = new AbortController();
  const response = await h.api.fetch(EXEC_URL, {
    method: "POST",
    headers: { "content-type": "application/connect+proto" },
    body: REQUEST_BODY,
    signal: controller.signal,
  });
  const reader = response.body!.getReader();
  const readFrame = async (): Promise<Read> => {
    const { value, done } = await reader.read();
    return { at: h.clock.now(), flags: done ? undefined : value![0] };
  };
  return { controller, reader, readFrame };
}

describe("FakeApi stream scripts", () => {
  test("delivers the next message after consecutive Sleep steps", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield new Sleep(100);
      yield new Sleep(100);
      yield execMsg(ev.exited(0));
    });
    const { readFrame } = await open(h);

    const reads: Read[] = [];
    // accepted, started, exited, then the end-of-stream message.
    for (let i = 0; i < 4; i++) {
      reads.push(await h.clock.run(readFrame()));
    }
    expect(reads.map((r) => r.flags)).toEqual([0, 0, 0, 2]);
    expect(reads.map((r) => r.at)).toEqual([0, 0, 200, 200]);
    expect(await h.clock.run(readFrame())).toMatchObject({
      flags: undefined,
    });
  });

  test("delivers the first message after consecutive Sleep steps", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", function* () {
      yield new Sleep(50);
      yield new Sleep(50);
      yield new Sleep(50);
      yield execMsg(ev.accepted());
    });
    const { readFrame } = await open(h);

    const first = await h.clock.run(readFrame());
    expect(first).toEqual({ at: 150, flags: 0 });
  });

  test("keeps a single Sleep, Eof and Disconnect working", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", function* () {
      yield new Sleep(30);
      yield execMsg(ev.accepted());
      yield new Sleep(30);
      yield new Eof();
    });
    const eof = await open(h);
    expect(await h.clock.run(eof.readFrame())).toEqual({ at: 30, flags: 0 });
    expect(await h.clock.run(eof.readFrame())).toEqual({
      at: 60,
      flags: undefined,
    });

    h.api.stream(RelayService, "Exec", function* () {
      yield new Sleep(10);
      yield new Sleep(10);
      yield new Disconnect();
    });
    const dropped = await open(h);
    await expect(h.clock.run(dropped.readFrame())).rejects.toThrow(
      "terminated",
    );
  });

  test("turns a script error after consecutive Sleep steps into an end-of-stream error", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", function* () {
      yield new Sleep(10);
      yield new Sleep(10);
      throw new Error("boom");
    });
    const { readFrame } = await open(h);

    expect(await h.clock.run(readFrame())).toEqual({ at: 20, flags: 2 });
  });

  test("stops the sleep loop when the call is aborted during a Sleep", async () => {
    const h = setup();
    let reachedAfterSleeps = false;
    h.api.stream(RelayService, "Exec", function* () {
      yield execMsg(ev.accepted());
      yield new Sleep(100);
      yield new Sleep(100);
      reachedAfterSleeps = true;
      yield execMsg(ev.exited(0));
    });
    const { controller, readFrame } = await open(h);

    expect((await h.clock.run(readFrame())).flags).toBe(0);
    const pending = readFrame();
    await h.clock.advance(50);
    controller.abort(new Error("caller canceled"));
    await h.clock.settle();
    // The script must not run past the sleeps once the call is aborted.
    await h.clock.advance(1_000);
    expect(reachedAfterSleeps).toBe(false);
    pending.catch(() => {});
  });

  test("stops the sleep loop when the reader cancels during a Sleep", async () => {
    const h = setup();
    let reachedAfterSleeps = false;
    h.api.stream(RelayService, "Exec", function* () {
      yield new Sleep(100);
      yield new Sleep(100);
      reachedAfterSleeps = true;
      yield execMsg(ev.accepted());
    });
    const { reader, readFrame } = await open(h);

    const pending = readFrame();
    await h.clock.advance(50);
    await reader.cancel();
    await h.clock.advance(1_000);
    expect(reachedAfterSleeps).toBe(false);
    await pending;
  });

  test("still ends a Hang step when the call is aborted", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", function* () {
      yield new Sleep(10);
      yield new Hang();
    });
    const { controller, readFrame } = await open(h);

    const pending = readFrame();
    pending.catch(() => {});
    await h.clock.advance(20);
    controller.abort(new Error("caller canceled"));
    await expect(pending).rejects.toThrow("caller canceled");
  });
});

describe("FakeApi cancellation before dispatch", () => {
  test("rejects an already aborted unary call without dispatching it", async () => {
    const h = setup();
    const indexes: number[] = [];
    h.api.unary(SandboxService, "GetSandbox", (_req, call) => {
      indexes.push(call.index);
      return { sandbox: {} };
    });
    const reason = new Error("caller canceled");

    await expect(getSandbox(h, AbortSignal.abort(reason))).rejects.toBe(reason);

    expect(indexes).toEqual([]);
    expect(h.api.callsOf("GetSandbox")).toHaveLength(0);
    expect(h.api.fetchedUrls).toEqual([GET_SANDBOX_URL]);

    // The canceled call consumed no index: the next call is the first one.
    const response = await getSandbox(h);
    expect(response.status).toBe(200);
    expect(indexes).toEqual([0]);
    expect(h.api.callsOf("GetSandbox")).toHaveLength(1);
  });

  test("rejects an already aborted streaming call without dispatching it", async () => {
    const h = setup();
    const indexes: number[] = [];
    h.api.stream(RelayService, "Exec", function* (_req, call) {
      indexes.push(call.index);
      yield execMsg(ev.accepted());
    });
    const reason = new Error("caller canceled");

    await expect(openExec(h, AbortSignal.abort(reason))).rejects.toBe(reason);

    expect(indexes).toEqual([]);
    expect(h.api.callsOf("Exec")).toHaveLength(0);
    expect(h.api.fetchedUrls).toEqual([EXEC_URL]);

    // The next live call starts the script at index 0 and streams normally.
    const response = await openExec(h);
    const reader = response.body!.getReader();
    const first = await h.clock.run(reader.read());
    expect(first.value![0]).toBe(0);
    expect(indexes).toEqual([0]);
    expect(h.api.callsOf("Exec")).toHaveLength(1);
  });

  test("dispatches nothing when a unary call aborts during the body read", async () => {
    const h = setup();
    const indexes: number[] = [];
    h.api.unary(SandboxService, "GetSandbox", (_req, call) => {
      indexes.push(call.index);
      return { sandbox: {} };
    });
    const controller = new AbortController();
    const reason = new Error("aborted while reading the body");

    const pending = getSandbox(h, controller.signal);
    // The body read has not finished yet: fetch is suspended at it.
    controller.abort(reason);

    await expect(pending).rejects.toBe(reason);
    expect(indexes).toEqual([]);
    expect(h.api.callsOf("GetSandbox")).toHaveLength(0);

    await getSandbox(h);
    expect(indexes).toEqual([0]);
  });

  test("dispatches nothing when a streaming call aborts during the body read", async () => {
    const h = setup();
    const indexes: number[] = [];
    h.api.stream(RelayService, "Exec", function* (_req, call) {
      indexes.push(call.index);
      yield execMsg(ev.accepted());
    });
    const controller = new AbortController();
    const reason = new Error("aborted while reading the body");

    const pending = openExec(h, controller.signal);
    controller.abort(reason);

    await expect(pending).rejects.toBe(reason);
    expect(indexes).toEqual([]);
    expect(h.api.callsOf("Exec")).toHaveLength(0);
  });
});
