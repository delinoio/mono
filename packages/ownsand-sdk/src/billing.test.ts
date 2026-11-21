// SPDX-License-Identifier: Apache-2.0
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { billingReasonOf, DataPathError, isBillingError } from "./errors.ts";
import { BillingService } from "./gen/delino/ownsand/v1/billing_pb.ts";
import {
  BillingReason,
  SandboxState,
} from "./gen/delino/ownsand/v1/common_pb.ts";
import { DataPathService } from "./gen/delino/ownsand/v1/data_path_pb.ts";
import { RelayService } from "./gen/delino/ownsand/v1/relay_pb.ts";
import { SandboxService } from "./gen/delino/ownsand/v1/sandbox_pb.ts";
import { Disconnect, ownsandError } from "./testing/fake-api.ts";
import {
  attachMsg,
  ev,
  execMsg,
  ORG,
  SANDBOX,
  setup,
} from "./testing/harness.ts";

const MESSAGE =
  "the organization is suspended for billing; manage billing at https://accounts.delino.io";

const REASONS = [
  BillingReason.ACCOUNT_REQUIRED,
  BillingReason.BLOCKED,
  BillingReason.SUSPENDED,
] as const;

const HOUR = 3_600_000;

const validCreate = {
  source: { baseImage: 1 },
  resources: { vcpuCount: 1, memoryMib: 1024, diskGib: 10 },
  idleTtl: HOUR,
  maxTtl: 2 * HOUR,
} as const;

function billingError(reason: BillingReason): ConnectError {
  return ownsandError(Code.ResourceExhausted, MESSAGE, {
    billingReason: reason,
  });
}

function expectBillingError(error: unknown, reason: BillingReason): void {
  expect(billingReasonOf(error)).toBe(reason);
  expect(isBillingError(error)).toBe(true);
  // The server message with the billing link reaches the caller.
  expect(String((error as Error).message)).toContain(
    "https://accounts.delino.io",
  );
}

describe("billing error helpers", () => {
  test("read the reason of a billing error, also through a cause chain", () => {
    for (const reason of REASONS) {
      const error = billingError(reason);
      expect(billingReasonOf(error)).toBe(reason);
      const wrapped = new DataPathError("data path failed", {
        code: Code.ResourceExhausted,
        cause: error,
      });
      expect(billingReasonOf(wrapped)).toBe(reason);
      expect(isBillingError(wrapped)).toBe(true);
    }
  });

  test("report no reason for errors that are not billing errors", () => {
    const others: unknown[] = [
      undefined,
      "text",
      new Error("x"),
      new ConnectError("agent at capacity", Code.ResourceExhausted),
      ownsandError(Code.ResourceExhausted, "slow down", {
        retryAfterMs: 1000,
      }),
      ownsandError(Code.PermissionDenied, "no", {
        billingReason: BillingReason.BLOCKED,
      }),
    ];
    for (const error of others) {
      expect(billingReasonOf(error)).toBe(BillingReason.UNSPECIFIED);
      expect(isBillingError(error)).toBe(false);
    }
  });
});

describe("billing errors end operations without a retry", () => {
  for (const reason of REASONS) {
    test(`createSandbox with ${BillingReason[reason]}`, async () => {
      const h = setup();
      h.api.unary(SandboxService, "CreateSandbox", () => {
        throw billingError(reason);
      });
      const error = await h.clock
        .run(h.client.createSandbox(validCreate))
        .catch((e: unknown) => e);
      expectBillingError(error, reason);
      expect(h.api.callsOf("CreateSandbox")).toHaveLength(1);
    });

    test(`AuthorizeRelay with ${BillingReason[reason]}`, async () => {
      const h = setup();
      h.api.unary(DataPathService, "AuthorizeRelay", () => {
        throw billingError(reason);
      });
      const error = await h.clock
        .run(
          h.client
            .sandbox(SANDBOX)
            .exec({ command: ["x"] })
            .wait(),
        )
        .catch((e: unknown) => e);
      expectBillingError(error, reason);
      expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(1);
      expect(h.api.callsOf("Exec")).toHaveLength(0);
    });
  }

  test("a relay Exec stream", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", function* () {
      throw billingError(BillingReason.SUSPENDED);
    });
    const error = await h.clock
      .run(
        h.client
          .sandbox(SANDBOX)
          .exec({ command: ["x"] })
          .wait(),
      )
      .catch((e: unknown) => e);
    expectBillingError(error, BillingReason.SUSPENDED);
    expect(h.api.callsOf("Exec")).toHaveLength(1);
    expect(h.api.callsOf("AttachExec")).toHaveLength(0);
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(1);
  });

  test("a relay ReadFile stream", async () => {
    const h = setup();
    h.api.stream(RelayService, "ReadFile", function* () {
      throw billingError(BillingReason.SUSPENDED);
    });
    const error = await h.clock
      .run(h.client.sandbox(SANDBOX).readFileBytes("/f"))
      .catch((e: unknown) => e);
    expectBillingError(error, BillingReason.SUSPENDED);
    expect(h.api.callsOf("ReadFile")).toHaveLength(1);
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(1);
  });

  test("a relay WriteFile call", async () => {
    const h = setup();
    h.api.unary(RelayService, "WriteFile", () => {
      throw billingError(BillingReason.SUSPENDED);
    });
    const error = await h.clock
      .run(h.client.sandbox(SANDBOX).writeFile("/f", "hello"))
      .catch((e: unknown) => e);
    expectBillingError(error, BillingReason.SUSPENDED);
    expect(h.api.callsOf("WriteFile")).toHaveLength(1);
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(1);
  });

  test("the AuthorizeRelay renewal of relay recovery", async () => {
    const h = setup();
    h.api.unary(DataPathService, "AuthorizeRelay", (_req, call) => {
      if (call.index > 0) {
        throw billingError(BillingReason.SUSPENDED);
      }
      return { grantId: "grant-1", expiresIn: { seconds: 900n } };
    });
    h.api.stream(RelayService, "Exec", async function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield execMsg(ev.stdout(0, "abc"));
      yield new Disconnect();
    });
    const error = await h.clock
      .run(
        h.client
          .sandbox(SANDBOX)
          .exec({ command: ["x"] })
          .wait(),
      )
      .catch((e: unknown) => e);
    expectBillingError(error, BillingReason.SUSPENDED);
    // The first grant and one renewal; no retry of the renewal.
    expect(h.api.callsOf("AuthorizeRelay")).toHaveLength(2);
    expect(h.api.callsOf("AttachExec")).toHaveLength(0);
  });

  test("the continuation stream of relay recovery", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", async function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield execMsg(ev.stdout(0, "abc"));
      yield new Disconnect();
    });
    h.api.stream(RelayService, "AttachExec", function* () {
      throw billingError(BillingReason.SUSPENDED);
    });
    const error = await h.clock
      .run(
        h.client
          .sandbox(SANDBOX)
          .exec({ command: ["x"] })
          .wait(),
      )
      .catch((e: unknown) => e);
    expectBillingError(error, BillingReason.SUSPENDED);
    expect(h.api.callsOf("Exec")).toHaveLength(1);
    expect(h.api.callsOf("AttachExec")).toHaveLength(1);
  });
});

describe("other resource_exhausted errors are still retried", () => {
  test("a rate-limited createSandbox", async () => {
    const h = setup();
    h.api.unary(SandboxService, "CreateSandbox", (_req, call) => {
      if (call.index === 0) {
        throw ownsandError(Code.ResourceExhausted, "rate limited", {
          retryAfterMs: 1000,
        });
      }
      return { sandbox: { sandboxId: SANDBOX, state: SandboxState.CREATING } };
    });
    await h.clock.run(h.client.createSandbox(validCreate));
    expect(h.api.callsOf("CreateSandbox")).toHaveLength(2);
  });

  test("an AttachExec continuation at agent capacity", async () => {
    const h = setup();
    h.api.stream(RelayService, "Exec", async function* () {
      yield execMsg(ev.accepted());
      yield execMsg(ev.started());
      yield new Disconnect();
    });
    h.api.stream(RelayService, "AttachExec", async function* (_req, call) {
      if (call.index === 0) {
        throw ownsandError(Code.ResourceExhausted, "agent stream capacity");
      }
      yield attachMsg(ev.accepted());
      yield attachMsg(ev.exited(0));
    });
    const result = await h.clock.run(
      h.client
        .sandbox(SANDBOX)
        .exec({ command: ["x"] })
        .wait(),
    );
    expect(result.kind).toBe("exited");
    expect(h.api.callsOf("AttachExec")).toHaveLength(2);
  });
});

describe("billing service client", () => {
  test("fills the default organization", async () => {
    const h = setup();
    h.api.unary(BillingService, "GetBillingStatus", (req) => ({
      billing: { phase: 1, linked: req.organizationId === ORG },
    }));
    const res = await h.clock.run(h.client.billing.getBillingStatus({}));
    expect(res.billing?.linked).toBe(true);
    expect(
      (h.api.callsOf("GetBillingStatus")[0]!.request as any).organizationId,
    ).toBe(ORG);
  });
});
