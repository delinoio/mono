// SPDX-License-Identifier: Apache-2.0
import { spawn } from "node:child_process";
import { createServer, type Server } from "node:net";
import { fileURLToPath } from "node:url";
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";

// The fixed development port that `rspress.config.ts` declares.
const FIXED_PORT = 13500;
const LOOPBACK_HOST = "127.0.0.1";
const EXIT_TIMEOUT_MS = 60_000;
const APP_DIRECTORY = fileURLToPath(new URL("..", import.meta.url));

interface DevServerResult {
  exitCode: number | null;
  output: string;
}

// Holds a loopback port so a development server cannot bind it. A port that
// another process already holds is just as occupied, so `EADDRINUSE` is not a
// failure: the conflict the test needs exists either way.
const occupy = (port: number): Promise<Server | undefined> =>
  new Promise((resolve, reject) => {
    const holder = createServer();
    holder.once("error", (error: NodeJS.ErrnoException) => {
      if (error.code === "EADDRINUSE") {
        resolve(undefined);
        return;
      }
      reject(error);
    });
    holder.listen(port, LOOPBACK_HOST, () => resolve(holder));
  });

const release = (holder: Server | undefined): Promise<void> =>
  new Promise((resolve) => {
    if (holder === undefined) {
      resolve();
      return;
    }
    holder.close(() => resolve());
  });

// Runs `rspress dev` as the e2e runner does, from the app directory with an
// argument array and no shell, and waits for it to exit. A server that
// survives the timeout counts as a failure of the rule under test and is
// killed.
const runDevServer = (args: string[]): Promise<DevServerResult> =>
  new Promise((resolve, reject) => {
    const child = spawn("rspress", ["dev", ...args], {
      cwd: APP_DIRECTORY,
      shell: false,
      stdio: ["ignore", "pipe", "pipe"],
    });
    let output = "";
    child.stdout.on("data", (chunk: Buffer) => (output += chunk.toString()));
    child.stderr.on("data", (chunk: Buffer) => (output += chunk.toString()));
    const timer = setTimeout(() => {
      child.kill("SIGKILL");
      reject(
        new Error(`rspress dev did not exit within ${EXIT_TIMEOUT_MS} ms`, {
          cause: output,
        }),
      );
    }, EXIT_TIMEOUT_MS);
    child.once("error", (error) => {
      clearTimeout(timer);
      reject(error);
    });
    child.once("close", (exitCode) => {
      clearTimeout(timer);
      resolve({ exitCode, output });
    });
  });

describe("development server port", () => {
  test("exits on a conflict at the fixed port instead of moving to another port", async () => {
    // No `--port` flag: the port comes from `rspress.config.ts`, so this also
    // checks the fixed value. The e2e runner itself passes an explicit port.
    const holder = await occupy(FIXED_PORT);
    try {
      const result = await runDevServer([]);

      expect(result.exitCode).not.toBe(0);
      expect(result.output).toContain(`Port ${FIXED_PORT} is occupied`);
      expect(result.output).not.toContain("Local:");
    } finally {
      await release(holder);
    }
  });

  test("exits on a conflict at an explicit port", async () => {
    // `strictPort` stays in force when a flag selects the port, which is how
    // the runner starts the server on a free port.
    const holder = await occupy(0);
    try {
      const address = holder?.address();
      if (address === null || typeof address !== "object") {
        throw new Error("the port holder has no address");
      }

      const result = await runDevServer([
        "--host",
        LOOPBACK_HOST,
        "--port",
        String(address.port),
      ]);

      expect(result.exitCode).not.toBe(0);
      expect(result.output).toContain(`Port ${address.port} is occupied`);
    } finally {
      await release(holder);
    }
  });
});
