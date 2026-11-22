// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import { RelayService } from "@delino/ownsand-protos/v1/relay_pb";
import { Disconnect, Eof, Hang, Sleep } from "./fake-api.ts";
import { ev, execMsg, setup } from "./harness.ts";

const EXEC_URL = `https://ownsand.api.test/${RelayService.typeName}/Exec`;
// One enveloped, empty request message.
const REQUEST_BODY = new Uint8Array([0, 0, 0, 0, 0]);

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
