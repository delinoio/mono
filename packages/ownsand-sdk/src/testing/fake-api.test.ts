// SPDX-License-Identifier: Apache-2.0
import { getEventListeners } from "node:events";
import { describe, expect, test } from "@rstest/core";
import { RelayService } from "@delino/ownsand-protos/v1/relay_pb";
import { SandboxService } from "@delino/ownsand-protos/v1/sandbox_pb";
import { Disconnect, Eof, Hang, Sleep, Trickle } from "./fake-api.ts";
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

  describe.each([
    ["EOF", Eof],
    ["disconnect", Disconnect],
  ] as const)("scripted %s", (_name, TerminalStep) => {
    async function expectTerminalRead(
      read: Promise<ReadableStreamReadResult<Uint8Array>>,
    ) {
      if (TerminalStep === Eof) {
        await expect(read).resolves.toEqual({ done: true, value: undefined });
      } else {
        await expect(read).rejects.toBeInstanceOf(TypeError);
        await expect(read).rejects.toMatchObject({
          message: "terminated: other side closed",
        });
      }
    }

    test("closes the script without advancing past the terminal step", async () => {
      const h = setup();
      let advancedScript = false;
      let cleanups = 0;
      h.api.stream(RelayService, "Exec", function* () {
        try {
          yield execMsg(ev.accepted());
          yield new TerminalStep();
          advancedScript = true;
        } finally {
          cleanups++;
        }
      });
      const { reader, readFrame } = await open(h);

      expect((await h.clock.run(readFrame())).flags).toBe(0);
      await expectTerminalRead(h.clock.run(reader.read()));
      await h.clock.settle();

      expect(advancedScript).toBe(false);
      expect(cleanups).toBe(1);
    });

    test.each([
      ["returns", false],
      ["throws", true],
    ] as const)(
      "ends the body before an async finalizer %s",
      async (_outcome, throws) => {
        const h = setup();
        const gate = Promise.withResolvers<void>();
        let advancedScript = false;
        let cleanups = 0;
        h.api.stream(RelayService, "Exec", async function* () {
          try {
            yield execMsg(ev.accepted());
            yield new TerminalStep();
            advancedScript = true;
          } finally {
            await gate.promise;
            cleanups++;
            if (throws) throw new Error("script cleanup failed");
          }
        });
        const unhandled: unknown[] = [];
        const onUnhandled = (reason: unknown) => unhandled.push(reason);
        process.on("unhandledRejection", onUnhandled);
        try {
          const { reader, readFrame } = await open(h);
          expect((await h.clock.run(readFrame())).flags).toBe(0);
          await expectTerminalRead(h.clock.run(reader.read()));
          expect(cleanups).toBe(0);

          gate.resolve();
          await h.clock.settle();

          expect(advancedScript).toBe(false);
          expect(cleanups).toBe(1);
          await expectTerminalRead(h.clock.run(reader.read()));
          expect(unhandled).toEqual([]);
        } finally {
          gate.resolve();
          process.off("unhandledRejection", onUnhandled);
        }
      },
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
    let cleanups = 0;
    h.api.stream(RelayService, "Exec", function* () {
      try {
        yield execMsg(ev.accepted());
        yield new Sleep(100);
        yield new Sleep(100);
        reachedAfterSleeps = true;
        yield execMsg(ev.exited(0));
      } finally {
        cleanups++;
      }
    });
    const { controller, readFrame } = await open(h);

    expect((await h.clock.run(readFrame())).flags).toBe(0);
    const pending = readFrame();
    pending.catch(() => {});
    await h.clock.advance(50);
    const reason = new Error("caller canceled");
    controller.abort(reason);
    // The pending read rejects with the abort reason instead of hanging.
    await expect(pending).rejects.toBe(reason);
    // The script must not run past the sleeps once the call is aborted.
    await h.clock.advance(1_000);
    expect(reachedAfterSleeps).toBe(false);
    expect(cleanups).toBe(1);
  });

  test("errors the body of an aborted Sleep for a later read too", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", function* () {
      yield new Sleep(100);
      yield execMsg(ev.accepted());
    });
    const { controller, reader, readFrame } = await open(h);

    const pending = readFrame();
    pending.catch(() => {});
    const reason = new Error("caller canceled");
    controller.abort(reason);
    await expect(pending).rejects.toBe(reason);
    await expect(reader.read()).rejects.toBe(reason);
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

  test("releases Hang when only the reader cancels", async () => {
    const h = setup();
    let enteredHang = false;
    let advancedScript = false;
    let cleanups = 0;
    h.api.stream(RelayService, "Exec", function* () {
      try {
        enteredHang = true;
        yield new Hang();
        advancedScript = true;
        yield execMsg(ev.accepted());
      } finally {
        cleanups++;
      }
    });
    const { controller, reader } = await open(h);
    const pending = reader.read();
    await h.clock.settle();
    expect(enteredHang).toBe(true);

    await reader.cancel();

    await expect(pending).resolves.toEqual({ done: true, value: undefined });
    expect(h.clock.now()).toBe(0);
    expect(controller.signal.aborted).toBe(false);
    expect(getEventListeners(controller.signal, "abort")).toHaveLength(0);
    expect(advancedScript).toBe(false);
    expect(cleanups).toBe(1);
  });

  test("removes a Trickle delay when only the reader cancels", async () => {
    const h = setup();
    let advancedScript = false;
    let cleanups = 0;
    h.api.stream(RelayService, "Exec", function* () {
      try {
        yield new Trickle(execMsg(ev.accepted()), 3, 100);
        advancedScript = true;
        yield execMsg(ev.exited(0));
      } finally {
        cleanups++;
      }
    });
    const { controller, reader } = await open(h);
    expect((await reader.read()).done).toBe(false);
    const pending = reader.read();
    // A positive timer exists, but a zero budget keeps it from firing.
    await expect(h.clock.run(pending, 0)).rejects.toThrow("time limit reached");

    await reader.cancel();

    await expect(pending).resolves.toEqual({ done: true, value: undefined });
    await expect(h.clock.run(new Promise(() => {}), 0)).rejects.toThrow(
      "no timer is pending",
    );
    expect(h.clock.now()).toBe(0);
    expect(controller.signal.aborted).toBe(false);
    expect(getEventListeners(controller.signal, "abort")).toHaveLength(0);
    expect(cleanups).toBe(1);

    await h.clock.advance(500);
    await expect(reader.read()).resolves.toEqual({
      done: true,
      value: undefined,
    });
    expect(advancedScript).toBe(false);
    expect(cleanups).toBe(1);
  });

  test("rejects a read with the request's exact reason during a Trickle delay", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", function* () {
      yield new Trickle(execMsg(ev.accepted()), 3, 100);
    });
    const { controller, reader } = await open(h);
    expect((await reader.read()).done).toBe(false);
    const pending = reader.read();
    await expect(h.clock.run(pending, 0)).rejects.toThrow("time limit reached");
    const reason = new Error("caller canceled");

    controller.abort(reason);

    await expect(pending).rejects.toBe(reason);
    await expect(reader.read()).rejects.toBe(reason);
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
