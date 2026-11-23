// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { ExecResultKind } from "./operations/exec.ts";
import { RecoveryDeadlineError } from "./operations/common.ts";

import { Disconnect } from "./testing/fake-api.ts";
import {
  ev,
  execMsg,
  SANDBOX,
  setup,
  type Harness,
} from "./testing/harness.ts";
import { SandboxState } from "@delino/ownsand-protos/v1/common_pb";
import { RelayService } from "@delino/ownsand-protos/v1/relay_pb";
import { SandboxService } from "@delino/ownsand-protos/v1/sandbox_pb";

const MIN = 60_000;

/** Runs fn after ms of virtual time. */
function after(h: Harness, ms: number, fn: () => void): void {
  void h.clock.sleep(ms).then(fn);
}

function runExec(h: Harness, signal?: AbortSignal) {
  return h.client
    .sandbox(SANDBOX)
    .exec({ command: ["x"], signal })
    .wait();
}

/** An exec whose first send disconnects and whose AttachExec has no record. */
function disconnectThenNoRecord(h: Harness): void {
  h.api.stream(RelayService, "Exec", async function* (_req, call) {
    if (call.index === 0) {
      yield new Disconnect();
      return;
    }
    yield execMsg(ev.accepted());
    yield execMsg(ev.started());
    yield execMsg(ev.exited(0));
  });
  h.api.stream(RelayService, "AttachExec", async function* () {
    throw new ConnectError("no exec record", Code.NotFound);
  });
}

describe("the pre-start state lookup after AttachExec not_found", () => {
  test("a stalled GetSandbox ends at the recovery deadline with the last relay failure and sends no further Exec", async () => {
    const h = setup();
    disconnectThenNoRecord(h);
    let lookupSignal: AbortSignal | undefined;
    h.api.unary(SandboxService, "GetSandbox", (_req, call) => {
      lookupSignal = call.signal;
      return new Promise<never>(() => {});
    });
    const error = await h.clock.run(runExec(h)).catch((e) => e);
    expect(error).toBeInstanceOf(ConnectError);
    expect(error).not.toBeInstanceOf(RecoveryDeadlineError);
    expect(h.clock.now()).toBe(MIN);
    expect(h.api.callsOf("GetSandbox")).toHaveLength(1);
    expect(lookupSignal?.aborted).toBe(true);
    expect(h.api.callsOf("Exec")).toHaveLength(1);
  });

  test("a caller abort before the deadline cancels the lookup and returns the caller's reason exactly", async () => {
    const h = setup();
    disconnectThenNoRecord(h);
    let lookupSignal: AbortSignal | undefined;
    h.api.unary(SandboxService, "GetSandbox", (_req, call) => {
      lookupSignal = call.signal;
      return new Promise<never>(() => {});
    });
    const caller = new AbortController();
    const reason = new Error("caller gave up");
    const outcome = runExec(h, caller.signal).catch((e) => e);
    await h.clock.advance(30_000);
    expect(h.api.callsOf("GetSandbox")).toHaveLength(1);
    caller.abort(reason);
    expect(await outcome).toBe(reason);
    expect(h.clock.now()).toBe(30_000);
    expect(lookupSignal?.aborted).toBe(true);
    expect(h.api.callsOf("Exec")).toHaveLength(1);
  });

  test("a prompt RUNNING result resends the original request with the same exec ID", async () => {
    const h = setup();
    disconnectThenNoRecord(h);
    const result = await h.clock.run(runExec(h));
    expect(result.kind).toBe(ExecResultKind.Exited);
    const execs = h.api.callsOf("Exec").map((c) => c.request as any);
    expect(execs).toHaveLength(2);
    expect(execs[1]).toEqual(execs[0]);
    expect(execs[1].execId).not.toBe("");
  });

  test("a prompt PAUSED result still waits for the resume past the deadline", async () => {
    const h = setup();
    disconnectThenNoRecord(h);
    h.state.value = SandboxState.PAUSED;
    after(h, 3 * MIN, () => {
      h.state.value = SandboxState.RUNNING;
    });
    const result = await h.clock.run(runExec(h));
    expect(result.kind).toBe(ExecResultKind.Exited);
    expect(h.clock.now()).toBeGreaterThan(MIN);
    expect(h.api.callsOf("Exec")).toHaveLength(2);
  });

  test("a prompt lookup failure still resends the request while budget remains", async () => {
    const h = setup();
    disconnectThenNoRecord(h);
    h.api.unary(SandboxService, "GetSandbox", () => {
      throw new ConnectError("lookup failed", Code.Unavailable);
    });
    const result = await h.clock.run(runExec(h));
    expect(result.kind).toBe(ExecResultKind.Exited);
    expect(h.api.callsOf("Exec")).toHaveLength(2);
  });
});
