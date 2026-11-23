// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { LogEvent, type LogRecord } from "./logger.ts";
import { RelayService } from "@delino/ownsand-protos/v1/relay_pb";
import { SandboxEndReason } from "@delino/ownsand-protos/v1/common_pb";
import { Hang, Sleep } from "./testing/fake-api.ts";
import {
  attachMsg,
  ev,
  execMsg,
  file,
  SANDBOX,
  setup,
} from "./testing/harness.ts";

const READ = "delino.ownsand.v1.RelayService/ReadFile";
const EXEC = "delino.ownsand.v1.RelayService/Exec";
const ATTACH = "delino.ownsand.v1.RelayService/AttachExec";
const EXEC_ID = "0192f4b6-7c3a-7def-8abc-0123456789ac";

/** A client whose logger collects the `rpc` records of the relay streams. */
function loggedSetup() {
  const records: LogRecord[] = [];
  const h = setup({ logger: (r) => records.push(r) });
  const rpcs = (rpc: string): LogRecord[] =>
    records.filter((r) => r.event === LogEvent.Rpc && r.rpc === rpc);
  return { h, rpcs };
}

describe("relay stream rpc records", () => {
  describe("a stream that the consumer completes", () => {
    test("ReadFile logs exactly one ok record after the end event", async () => {
      const { h, rpcs } = loggedSetup();
      h.api.stream(RelayService, "ReadFile", function* () {
        yield file.accepted();
        yield file.chunk(0, "hello");
        yield new Sleep(250);
        yield file.end(5);
      });
      await h.clock.run(h.client.sandbox(SANDBOX).readFileBytes("/f"));
      await h.clock.settle();
      expect(rpcs(READ)).toEqual([
        { event: LogEvent.Rpc, rpc: READ, durationMs: 250, code: "ok" },
      ]);
    });

    test("Exec logs exactly one ok record for each terminal event", async () => {
      const failed = execMsg({
        case: "failed",
        value: { reason: 0, message: "no such file" },
      } as never);
      const ended = execMsg({
        case: "sandboxEnded",
        value: { reason: SandboxEndReason.UNSPECIFIED },
      } as never);
      for (const terminal of [execMsg(ev.exited(0)), failed, ended]) {
        const { h, rpcs } = loggedSetup();
        h.api.stream(RelayService, "Exec", function* () {
          yield execMsg(ev.accepted());
          yield execMsg(ev.started());
          yield new Sleep(120);
          yield terminal;
        });
        await h.clock.run(
          h.client
            .sandbox(SANDBOX)
            .exec({ command: ["x"] })
            .wait(),
        );
        await h.clock.settle();
        expect(rpcs(EXEC)).toEqual([
          { event: LogEvent.Rpc, rpc: EXEC, durationMs: 120, code: "ok" },
        ]);
      }
    });

    test("AttachExec logs exactly one ok record after the exit", async () => {
      const { h, rpcs } = loggedSetup();
      h.api.stream(RelayService, "AttachExec", function* () {
        yield attachMsg(ev.accepted());
        yield attachMsg(ev.started());
        yield new Sleep(80);
        yield attachMsg(ev.exited(0));
      });
      await h.clock.run(h.client.sandbox(SANDBOX).attachExec(EXEC_ID).wait());
      await h.clock.settle();
      expect(rpcs(ATTACH)).toEqual([
        { event: LogEvent.Rpc, rpc: ATTACH, durationMs: 80, code: "ok" },
      ]);
    });
  });

  describe("a stream that ends before a terminal event", () => {
    test("a caller abort logs Canceled", async () => {
      const { h, rpcs } = loggedSetup();
      h.api.stream(RelayService, "ReadFile", function* () {
        yield file.accepted();
        yield file.chunk(0, "hello");
        yield new Hang();
      });
      const controller = new AbortController();
      const reason = new Error("caller gave up");
      const iterator = h.client
        .sandbox(SANDBOX)
        .readFile("/f", { signal: controller.signal })
        [Symbol.asyncIterator]();
      expect((await h.clock.run(iterator.next())).done).toBe(false);
      const pending = iterator.next().then(
        () => undefined,
        (error: unknown) => error,
      );
      await h.clock.settle();
      controller.abort(reason);
      expect(await h.clock.run(pending)).toBe(reason);
      await h.clock.settle();
      expect(rpcs(READ)).toEqual([
        expect.objectContaining({ code: "Canceled" }),
      ]);
      expect(rpcs(READ)).toHaveLength(1);
    });

    test("a consumer that stops before the terminal event logs Canceled", async () => {
      const { h, rpcs } = loggedSetup();
      h.api.stream(RelayService, "ReadFile", function* () {
        yield file.accepted();
        yield file.chunk(0, "hello");
        yield file.chunk(5, "world");
        yield file.end(10);
      });
      await h.clock.run(
        (async () => {
          for await (const _ of h.client.sandbox(SANDBOX).readFile("/f")) {
            break;
          }
        })(),
      );
      await h.clock.settle();
      expect(rpcs(READ)).toEqual([
        expect.objectContaining({ code: "Canceled" }),
      ]);
    });

    test("a server error logs its Connect code", async () => {
      const { h, rpcs } = loggedSetup();
      h.api.stream(RelayService, "Exec", function* () {
        yield execMsg(ev.accepted());
        yield execMsg(ev.started());
        throw new ConnectError("denied", Code.PermissionDenied);
      });
      await expect(
        h.clock.run(
          h.client
            .sandbox(SANDBOX)
            .exec({ command: ["x"] })
            .wait(),
        ),
      ).rejects.toBeInstanceOf(Error);
      await h.clock.settle();
      expect(rpcs(EXEC)).toEqual([
        expect.objectContaining({ code: "PermissionDenied" }),
      ]);
    });
  });
});
