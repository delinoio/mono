// SPDX-License-Identifier: Apache-2.0
// End-to-end tests over a real HTTP/2 (h2c) connection to the fake control
// plane on the loopback interface, with the SDK's default HTTP/2 client.
import * as http2 from "node:http2";
import type { AddressInfo } from "node:net";

import { connectNodeAdapter } from "@connectrpc/connect-node";
import { afterEach, describe, expect, test } from "@rstest/core";

import { OwnSandClient } from "../src/client.ts";
import { systemClock } from "../src/clock.ts";
import { ExecEventKind, ExecResultKind } from "../src/relay/exec.ts";
import { API_KEY, FakeOwnSand, ORG, SANDBOX } from "./support/fake.ts";
import { DISCONNECT_HEADER } from "./support/faulty-client.ts";
import { text } from "./support/harness.ts";

interface Server {
  origin: string;
  fake: FakeOwnSand;
  headers: http2.IncomingHttpHeaders[];
  close(): Promise<void>;
}

/**
 * Serves the fake over h2c. A response that carries the fake's disconnect
 * marker is cut with RST_STREAM instead, so the client sees a real transport
 * interruption.
 */
async function serve(): Promise<Server> {
  const fake = new FakeOwnSand({
    ...systemClock,
    serverWallNow: () => Date.now(),
  });
  const adapter = connectNodeAdapter({ routes: fake.routes });
  const headers: http2.IncomingHttpHeaders[] = [];
  const server = http2.createServer((req, res) => {
    headers.push({ ...req.headers });
    const cut = () => req.stream.close(http2.constants.NGHTTP2_INTERNAL_ERROR);
    const writeHead = res.writeHead.bind(res) as (
      ...args: unknown[]
    ) => unknown;
    (
      res as unknown as { writeHead: (...args: unknown[]) => unknown }
    ).writeHead = (...args: unknown[]) => {
      if (JSON.stringify(args).includes(DISCONNECT_HEADER)) {
        cut();
        return res;
      }
      return writeHead(...args);
    };
    const write = res.write.bind(res) as (...args: unknown[]) => boolean;
    (res as unknown as { write: (...args: unknown[]) => boolean }).write = (
      chunk: unknown,
      ...rest: unknown[]
    ) => {
      if (
        chunk instanceof Uint8Array &&
        Buffer.from(chunk).includes(DISCONNECT_HEADER)
      ) {
        cut();
        return false;
      }
      return write(chunk, ...rest);
    };
    adapter(req, res);
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const port = (server.address() as AddressInfo).port;
  return {
    origin: `http://127.0.0.1:${port}`,
    fake,
    headers,
    close: () =>
      new Promise<void>((resolve) => {
        server.close(() => resolve());
      }),
  };
}

let cleanup: (() => Promise<void>)[] = [];
afterEach(async () => {
  for (const c of cleanup.splice(0)) {
    await c();
  }
});

async function setup(): Promise<{ server: Server; client: OwnSandClient }> {
  const server = await serve();
  const client = new OwnSandClient({
    origin: server.origin,
    apiKey: API_KEY,
    organizationId: ORG,
    env: {},
  });
  cleanup.push(async () => {
    client.close();
    await server.close();
  });
  return { server, client };
}

describe("relay over HTTP/2", () => {
  test("exec, write and read end to end with the credential and the grant header", async () => {
    const { server, client } = await setup();
    server.fake.programs.set("echo hi", [
      { delayMs: 5, stdout: "hi\n" },
      { delayMs: 5, exit: 0 },
    ]);
    const sandbox = client.sandbox(SANDBOX);
    const { result, stdout } = await sandbox.exec(["echo", "hi"]).collect();
    expect(result.kind).toBe(ExecResultKind.EXITED);
    expect(text(stdout)).toBe("hi\n");

    const data = new Uint8Array(1536 * 1024).map((_, i) => i % 251);
    await sandbox.writeFile("/out.bin", data);
    const back = await sandbox.readFile("/out.bin").bytes();
    expect(Buffer.from(back).equals(Buffer.from(data))).toBe(true);

    for (const h of server.headers) {
      expect(h["authorization"]).toBe(`Bearer ${API_KEY}`);
      expect(h["x-ownsand-sdk-activity"]).toBeUndefined();
    }
    const relay = server.headers.filter((h) =>
      String(h[":path"]).includes("RelayService"),
    );
    expect(relay.length).toBeGreaterThanOrEqual(4);
    expect(
      relay.every((h) => typeof h["ownsand-relay-grant-id"] === "string"),
    ).toBe(true);
  });

  test("an abrupt stream reset after delivered output continues with AttachExec", async () => {
    const { server, client } = await setup();
    server.fake.programs.set("p", [
      { delayMs: 5, stdout: "a" },
      { delayMs: 50, stdout: "b" },
      { delayMs: 5, exit: 0 },
    ]);
    server.fake.hooks.stream = (_rpc, n) =>
      n === 0 ? { disconnectAfterData: 1 } : undefined;
    const events = [];
    const run = client.sandbox(SANDBOX).exec(["p"]);
    for await (const ev of run) {
      events.push(ev);
    }
    expect(events.filter((e) => e.kind === ExecEventKind.STARTED)).toHaveLength(
      1,
    );
    expect(
      events
        .filter((e) => e.kind === ExecEventKind.STDOUT)
        .map((e) => text((e as { data: Uint8Array }).data)),
    ).toEqual(["a", "b"]);
    expect(server.fake.callsOf("AttachExec")[0]!.stdoutOffset).toBe(1n);
    expect(server.fake.execsRunTwice()).toEqual([]);
  });

  test("a reset AuthorizeRelay response is retried with the same authorization_id", async () => {
    const { server, client } = await setup();
    server.fake.hooks.authorize = (_req, n) =>
      n === 0 ? { loseResponse: true } : undefined;
    expect((await client.sandbox(SANDBOX).exec(["true"]).wait()).kind).toBe(
      ExecResultKind.EXITED,
    );
    const auth = server.fake.callsOf("AuthorizeRelay");
    expect(auth).toHaveLength(2);
    expect(auth[1]!.authorizationId).toBe(auth[0]!.authorizationId);
  });
});
