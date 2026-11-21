// SPDX-License-Identifier: Apache-2.0
//
// End-to-end test of the TypeScript SDK against a real control plane and a
// real device agent over the relay. It runs only when the OwnSand
// end-to-end suite (servers/ownsand-api/e2e) starts it and sets the
// OWNSAND_E2E_* variables below; otherwise it is skipped.
import { describe, expect, test } from "@rstest/core";
import {
  BaseImage,
  DataPathTransport,
  ExecEventType,
  ExecResultKind,
  NetworkMode,
  OwnSandClient,
  SandboxState,
  SnapshotState,
} from "./index.ts";

const env = process.env;
const apiUrl = env.OWNSAND_E2E_API_URL ?? "";
const apiKey = env.OWNSAND_E2E_API_KEY ?? "";
const org = env.OWNSAND_E2E_ORG ?? "";
const enabled = apiUrl !== "" && apiKey !== "" && org !== "";

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
  },
);
