// SPDX-License-Identifier: Apache-2.0
//
// End-to-end test of the TypeScript SDK against a real control plane and a
// real device agent over the relay. It runs only when the OwnSand
// end-to-end suite (servers/ownsand-api/e2e) starts it and sets the
// OWNSAND_E2E_* variables below; otherwise it is skipped.
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { DataPathService } from "@delino/ownsand-protos/v1/data_path_pb";
import { RelayService } from "@delino/ownsand-protos/v1/relay_pb";
import {
  BaseImage,
  BillingReason,
  billingReasonOf,
  DataPathTransport,
  ExecEventType,
  ExecResultKind,
  isBillingError,
  NetworkMode,
  OwnSandClient,
  type Sandbox,
  SandboxState,
  SnapshotState,
} from "./index.ts";

const env = process.env;
const apiUrl = env.OWNSAND_E2E_API_URL ?? "";
const apiKey = env.OWNSAND_E2E_API_KEY ?? "";
const org = env.OWNSAND_E2E_ORG ?? "";
const enabled = apiUrl !== "" && apiKey !== "" && org !== "";
// The billing control endpoint of the end-to-end suite. It changes the
// billing status of the organization through the fake delino-accounts.
const billingUrl = env.OWNSAND_E2E_BILLING_URL ?? "";

const decoder = new TextDecoder();

function randomBytes(n: number): Uint8Array {
  const out = new Uint8Array(n);
  for (let i = 0; i < n; i += 65536) {
    crypto.getRandomValues(out.subarray(i, Math.min(n, i + 65536)));
  }
  return out;
}

function sameBytes(a: Uint8Array, b: Uint8Array): boolean {
  return a.length === b.length && a.every((v, i) => v === b[i]);
}

/**
 * Billing statuses that the billing control endpoint applies. The values
 * are the path segments of servers/ownsand-api/e2e/tssdk_test.go.
 */
enum BillingControlStatus {
  Suspended = "suspended",
  Allowed = "allowed",
}

/** Applies a billing status to the organization at once. */
async function setBilling(status: BillingControlStatus): Promise<void> {
  const res = await fetch(`${billingUrl}/billing/${status}`, {
    method: "POST",
  });
  if (!res.ok) {
    throw new Error(
      `billing control ${status}: ${res.status} ${await res.text()}`,
    );
  }
}

/** Polls the sandbox until it reaches the state, for at most a minute. */
async function waitForState(
  sandbox: Sandbox,
  want: SandboxState,
): Promise<void> {
  const deadline = Date.now() + 60_000;
  for (;;) {
    const { state } = await sandbox.get();
    if (state === want) return;
    if (Date.now() > deadline) {
      throw new Error(
        `sandbox did not reach ${SandboxState[want]}: last ${SandboxState[state]}`,
      );
    }
    await new Promise((resolve) => setTimeout(resolve, 200));
  }
}

const AUTHORIZE_RELAY_PATH = `/${DataPathService.typeName}/${DataPathService.method.authorizeRelay.name}`;
const RELAY_SERVICE_PREFIX = `/${RelayService.typeName}/`;

/** Counts the API requests of a client by URL path. */
class RequestCounter {
  readonly #paths: string[] = [];

  readonly fetch: typeof globalThis.fetch = (input, init) => {
    const url =
      typeof input === "string"
        ? input
        : input instanceof URL
          ? input.href
          : input.url;
    this.#paths.push(new URL(url).pathname);
    return globalThis.fetch(input, init);
  };

  /** Forgets the requests counted so far. */
  reset(): void {
    this.#paths.length = 0;
  }

  /** The number of requests whose path starts with the prefix. */
  count(prefix: string): number {
    return this.#paths.filter((path) => path.startsWith(prefix)).length;
  }
}

/**
 * Runs a relay operation of a suspended organization and checks that it
 * fails at once with the terminal billing error: one rejected
 * `AuthorizeRelay` and no retry and no relay call.
 */
async function expectSuspended(
  requests: RequestCounter,
  operation: () => Promise<unknown>,
): Promise<void> {
  requests.reset();
  let caught: unknown;
  try {
    await operation();
  } catch (error) {
    caught = error;
  }
  expect(caught).toBeInstanceOf(ConnectError);
  expect((caught as ConnectError).code).toBe(Code.ResourceExhausted);
  expect(billingReasonOf(caught)).toBe(BillingReason.SUSPENDED);
  expect(isBillingError(caught)).toBe(true);
  expect((caught as ConnectError).message).toContain(
    "https://accounts.delino.io",
  );
  expect(requests.count(AUTHORIZE_RELAY_PATH)).toBe(1);
  expect(requests.count(RELAY_SERVICE_PREFIX)).toBe(0);
}

(enabled ? describe : describe.skip)(
  "OwnSand TypeScript SDK end to end",
  () => {
    test(
      "runs the sandbox lifecycle over the relay",
      async () => {
        const client = new OwnSandClient({
          origin: apiUrl,
          apiKey,
          organizationId: org,
          transport: DataPathTransport.RELAY,
          webrtc: false,
          env: {},
        });
        const sandbox = await client.createSandbox({
          source: { baseImage: BaseImage.LINUX },
          resources: { vcpuCount: 1, memoryMib: 512, diskGib: 1 },
          idleTtl: 30 * 60_000,
          maxTtl: 2 * 60 * 60_000,
          network: NetworkMode.NONE,
        });
        try {
          const info = await sandbox.waitUntilStable({ pollMs: 200 });
          expect(info.state).toBe(SandboxState.RUNNING);

          const run = await sandbox.run({
            command: ["/bin/sh", "-c", "echo out; echo err >&2; exit 4"],
          });
          expect(decoder.decode(run.stdout)).toBe("out\n");
          expect(decoder.decode(run.stderr)).toBe("err\n");
          expect(run.result.kind).toBe(ExecResultKind.Exited);
          if (run.result.kind === ExecResultKind.Exited) {
            expect(run.result.exitCode).toBe(4n);
          }

          // The process runtime's guest paths are host paths below the
          // sandbox root, which is the exec working directory.
          const pwd = await sandbox.run({ command: ["/bin/pwd"] });
          const root = decoder.decode(pwd.stdout).trim().replace(/\/$/, "");
          expect(root.startsWith("/")).toBe(true);

          const data = randomBytes(2 * 1024 * 1024 + 4321);
          await sandbox.writeFile(`${root}/ts.bin`, data);
          const read = await sandbox.readFileBytes(`${root}/ts.bin`);
          expect(sameBytes(read, data)).toBe(true);

          // Detach after the first output, then attach from its offset.
          const handle = sandbox.exec({
            command: ["/bin/sh", "-c", "echo one; sleep 2; echo two; exit 3"],
          });
          let first = "";
          for await (const event of handle) {
            if (event.type === ExecEventType.Stdout) {
              first = decoder.decode(event.data);
              break;
            }
          }
          expect(first).toBe("one\n");
          const attached = sandbox.attachExec(handle.execId, {
            stdoutOffset: BigInt(first.length),
          });
          let rest = "";
          for await (const event of attached) {
            if (event.type === ExecEventType.Stdout) {
              rest += decoder.decode(event.data);
            }
          }
          const result = await attached.wait();
          expect(rest).toBe("two\n");
          expect(result.kind).toBe(ExecResultKind.Exited);
          if (result.kind === ExecResultKind.Exited) {
            expect(result.exitCode).toBe(3n);
          }

          expect((await sandbox.pause()).state).toBe(SandboxState.PAUSED);
          expect((await sandbox.resume()).state).toBe(SandboxState.RUNNING);

          const snapshot = await sandbox.snapshot();
          for (;;) {
            const s = await client.getSnapshot(snapshot.snapshotId);
            if (s.state === SnapshotState.READY) break;
            expect(s.state).not.toBe(SnapshotState.FAILED);
            await new Promise((resolve) => setTimeout(resolve, 200));
          }
          await client.deleteSnapshot(snapshot.snapshotId);
        } finally {
          const deleted = await sandbox.delete();
          sandbox.close();
          expect([SandboxState.DELETING, SandboxState.DELETED]).toContain(
            deleted.state,
          );
        }
      },
      3 * 60_000,
    );

    (billingUrl === "" ? test.skip : test)(
      "ends relay operations of a suspended organization without a retry",
      async () => {
        const requests = new RequestCounter();
        const client = new OwnSandClient({
          origin: apiUrl,
          apiKey,
          organizationId: org,
          transport: DataPathTransport.RELAY,
          webrtc: false,
          fetch: requests.fetch,
          env: {},
        });
        const sandbox = await client.createSandbox({
          source: { baseImage: BaseImage.LINUX },
          resources: { vcpuCount: 1, memoryMib: 512, diskGib: 1 },
          idleTtl: 30 * 60_000,
          maxTtl: 2 * 60 * 60_000,
          network: NetworkMode.NONE,
        });
        try {
          expect((await sandbox.waitUntilStable({ pollMs: 200 })).state).toBe(
            SandboxState.RUNNING,
          );
          const pwd = await sandbox.run({ command: ["/bin/pwd"] });
          const root = decoder.decode(pwd.stdout).trim().replace(/\/$/, "");
          const path = `${root}/billing.bin`;
          const data = randomBytes(4096);
          await sandbox.writeFile(path, data);

          // The suspension pauses the running sandbox and rejects the data
          // path. Each relay operation ends with its first AuthorizeRelay.
          await setBilling(BillingControlStatus.Suspended);
          await waitForState(sandbox, SandboxState.PAUSED);
          await expectSuspended(requests, () =>
            sandbox.run({ command: ["/bin/echo", "suspended"] }),
          );
          await expectSuspended(requests, () => sandbox.readFileBytes(path));

          // The recovery resumes the sandbox that billing paused, and the
          // same operations work again.
          await setBilling(BillingControlStatus.Allowed);
          await waitForState(sandbox, SandboxState.RUNNING);
          const run = await sandbox.run({
            command: ["/bin/echo", "recovered"],
          });
          expect(decoder.decode(run.stdout)).toBe("recovered\n");
          expect(run.result.kind).toBe(ExecResultKind.Exited);
          expect(sameBytes(await sandbox.readFileBytes(path), data)).toBe(true);
        } finally {
          await setBilling(BillingControlStatus.Allowed);
          await sandbox.delete();
          sandbox.close();
        }
      },
      3 * 60_000,
    );
  },
);
