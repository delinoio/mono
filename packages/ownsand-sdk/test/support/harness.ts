// SPDX-License-Identifier: Apache-2.0
import { createConnectRouter } from "@connectrpc/connect";
import { createUniversalHandlerClient } from "@connectrpc/connect/protocol";

import { OwnSandClient, type OwnSandClientOptions } from "../../src/client.ts";
import type { LogRecord } from "../../src/log.ts";
import type { SandboxHandle } from "../../src/sandbox.ts";
import { VirtualClock } from "./clock.ts";
import { API_KEY, FakeOwnSand, ORG, SANDBOX } from "./fake.ts";
import { faultyClient } from "./faulty-client.ts";

export const ORIGIN = "https://ownsand.api.delino.io";

export interface Harness {
  clock: VirtualClock;
  fake: FakeOwnSand;
  client: OwnSandClient;
  sandbox: SandboxHandle;
  logs: LogRecord[];
  run<T>(p: Promise<T>): Promise<T>;
}

/** A client wired to an in-memory fake control plane on a virtual clock. */
export function harness(options: Partial<OwnSandClientOptions> = {}): Harness {
  const clock = new VirtualClock();
  const fake = new FakeOwnSand(clock);
  const router = createConnectRouter();
  fake.routes(router);
  const logs: LogRecord[] = [];
  const client = new OwnSandClient({
    origin: ORIGIN,
    apiKey: API_KEY,
    organizationId: ORG,
    clock,
    env: {},
    logger: (r) => logs.push(r),
    httpClient: faultyClient(
      createUniversalHandlerClient(router.handlers),
      clock,
    ),
    ...options,
  });
  return {
    clock,
    fake,
    client,
    get sandbox() {
      return client.sandbox(SANDBOX);
    },
    logs,
    run: (p) => clock.run(p),
  };
}

export const text = (b: Uint8Array) => new TextDecoder().decode(b);

/** Expects a promise to reject and returns the error. */
export async function rejection(
  p: Promise<unknown>,
): Promise<Error & { code?: number }> {
  try {
    await p;
  } catch (err) {
    return err as Error & { code?: number };
  }
  throw new Error("expected a rejection");
}
