// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";
import { OwnSandValidationError } from "./errors.ts";
import { RelayService } from "@delino/ownsand-protos/v1/relay_pb";
import { Hang } from "./testing/fake-api.ts";
import { attachMsg, ev, file, SANDBOX, setup } from "./testing/harness.ts";

// The largest value that the signed 64-bit wire field of an offset can carry.
const MAX_INT64 = 2n ** 63n - 1n;
const EXEC_ID = "0192f4b6-8000-7000-8000-000000000001";
const RULE = "must not exceed 9223372036854775807";

/** Runs `call` and returns what it throws synchronously, if anything. */
function thrown(call: () => unknown): unknown {
  try {
    call();
  } catch (e) {
    return e;
  }
  return undefined;
}

describe("an offset above the signed 64-bit range", () => {
  test.each([
    ["2^63", 2n ** 63n],
    ["2^64", 2n ** 64n],
    ["2^200", 2n ** 200n],
  ])("rejects a readFile offset of %s synchronously", (_name, offset) => {
    const h = setup();
    const error = thrown(() =>
      h.client.sandbox(SANDBOX).readFile("/f", { offset }),
    );
    expect(error).toBeInstanceOf(OwnSandValidationError);
    expect((error as OwnSandValidationError).field).toBe("offset");
    expect((error as OwnSandValidationError).message).toContain(RULE);
    expect(h.api.calls).toHaveLength(0);
  });

  test("reports the path before an oversized readFile offset", () => {
    const h = setup();
    const error = thrown(() =>
      h.client.sandbox(SANDBOX).readFile("relative", { offset: 2n ** 63n }),
    );
    expect((error as OwnSandValidationError).field).toBe("path");
    expect(h.api.calls).toHaveLength(0);
  });

  test("rejects a readFileBytes offset above the range before any request", async () => {
    const h = setup();
    await expect(
      h.client
        .sandbox(SANDBOX)
        .readFileBytes("/f", { offset: 2n ** 63n } as never),
    ).rejects.toBeInstanceOf(OwnSandValidationError);
    expect(h.api.calls).toHaveLength(0);
  });

  test.each([
    ["stdout_offset", { stdoutOffset: 2n ** 63n }],
    ["stderr_offset", { stderrOffset: 2n ** 63n }],
    ["stdout_offset", { stdoutOffset: 2n ** 64n, stderrOffset: 2n ** 64n }],
  ])(
    "rejects an attachExec %s above the range synchronously",
    (field, options) => {
      const h = setup();
      const error = thrown(() =>
        h.client.sandbox(SANDBOX).attachExec(EXEC_ID, options),
      );
      expect(error).toBeInstanceOf(OwnSandValidationError);
      expect((error as OwnSandValidationError).field).toBe(field);
      expect((error as OwnSandValidationError).message).toContain(RULE);
      expect(h.api.calls).toHaveLength(0);
    },
  );

  test("keeps the validation order of the exec ID and the offsets", () => {
    const h = setup();
    const badId = thrown(() =>
      h.client.sandbox(SANDBOX).attachExec("x", { stdoutOffset: 2n ** 63n }),
    );
    expect((badId as OwnSandValidationError).field).toBe("exec_id");
    const negative = thrown(() =>
      h.client
        .sandbox(SANDBOX)
        .attachExec(EXEC_ID, { stdoutOffset: -1n, stderrOffset: 2n ** 63n }),
    );
    expect((negative as OwnSandValidationError).field).toBe("stdout_offset");
    expect((negative as OwnSandValidationError).message).toContain(
      "must not be negative",
    );
    const stdoutFirst = thrown(() =>
      h.client.sandbox(SANDBOX).attachExec(EXEC_ID, {
        stdoutOffset: 2n ** 63n,
        stderrOffset: 2n ** 64n,
      }),
    );
    expect((stdoutFirst as OwnSandValidationError).field).toBe("stdout_offset");
    expect(h.api.calls).toHaveLength(0);
  });
});

describe("the largest signed 64-bit offset", () => {
  test("passes the readFile preflight and reaches the wire", async () => {
    const h = setup();
    const reason = new Error("stop after the request");
    const controller = new AbortController();
    // The agent cannot end a read at this offset with a representable size, so
    // the caller aborts once the request has reached the stream.
    h.api.stream(RelayService, "ReadFile", function* () {
      controller.abort(reason);
      yield file.accepted();
      yield new Hang();
    });
    const iterable = h.client
      .sandbox(SANDBOX)
      .readFile("/f", { offset: MAX_INT64, signal: controller.signal });
    await expect(
      h.clock.run(
        (async () => {
          for await (const _chunk of iterable) {
            // The stream carries no data.
          }
        })(),
      ),
    ).rejects.toBe(reason);
    const reads = h.api.callsOf("ReadFile").map((c) => c.request as any);
    expect(reads.length).toBeGreaterThan(0);
    expect(reads[0].offset).toBe(MAX_INT64);
  });

  test("passes the attachExec preflight and reaches the wire", async () => {
    const h = setup();
    h.api.stream(RelayService, "AttachExec", async function* () {
      yield attachMsg(ev.accepted());
      yield attachMsg(ev.exited(0));
    });
    const handle = h.client.sandbox(SANDBOX).attachExec(EXEC_ID, {
      stdoutOffset: MAX_INT64,
      stderrOffset: MAX_INT64,
    });
    await h.clock.run(handle.wait());
    const attach = h.api.callsOf("AttachExec")[0]!.request as any;
    expect(attach.stdoutOffset).toBe(MAX_INT64);
    expect(attach.stderrOffset).toBe(MAX_INT64);
  });
});
