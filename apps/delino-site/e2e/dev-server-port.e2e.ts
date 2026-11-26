// SPDX-License-Identifier: Apache-2.0
import { spawn } from "node:child_process";
import { createServer, type Server } from "node:net";
import { setTimeout as delay } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import { describe, test } from "@e2e-dev/web";
import { expect } from "e2e";
import { rspressBin } from "./rspress-bin";

// The fixed development port that `rspress.config.ts` declares.
const FIXED_PORT = 13600;
const LOOPBACK_HOST = "127.0.0.1";
const BIND_TIMEOUT_MS = 1_000;
const BIND_RETRY_MS = 50;
const EXIT_TIMEOUT_MS = 60_000;
const APP_DIRECTORY = fileURLToPath(new URL("..", import.meta.url));

interface DevServerResult {
  exitCode: number | null;
  output: string;
}

const listen = (port: number): Promise<Server> =>
  new Promise((resolve, reject) => {
    const holder = createServer();
    holder.once("error", reject);
    holder.listen(port, LOOPBACK_HOST, () => {
      holder.off("error", reject);
      resolve(holder);
    });
  });

// The fixture must hold the port itself because an unrelated listener can
// close at any time before Rspress tries to bind.
const occupy = async (port: number): Promise<Server> => {
  const deadline = performance.now() + BIND_TIMEOUT_MS;
  while (true) {
    try {
      return await listen(port);
    } catch (error) {
      if (
        port === 0 ||
        !(error instanceof Error) ||
        !("code" in error) ||
        error.code !== "EADDRINUSE"
      ) {
        throw error;
      }
      const remaining = deadline - performance.now();
      if (remaining > 0) {
        await delay(Math.min(BIND_RETRY_MS, remaining));
      }
      if (performance.now() >= deadline) {
        throw new Error(
          `Could not bind the test listener to ${LOOPBACK_HOST}:${port} within ${BIND_TIMEOUT_MS} ms`,
          {
            cause: error,
          },
        );
      }
    }
  }
};

const release = (holder: Server): Promise<void> =>
  new Promise((resolve) => {
    holder.close(() => resolve());
  });

// The host variables the e2e runner passes to the app command: `PATH`, `HOME`
// and the temporary-directory variables. The child gets only these, so a
// variable such as `PORTLESS_URL` or `PORT` in the parent environment cannot
// make `rspress.config.ts` select another port than the one under test.
const HOST_ENV_NAMES = ["PATH", "HOME", "TMPDIR", "TMP", "TEMP"] as const;

const hostEnv = (): NodeJS.ProcessEnv => {
  const env: NodeJS.ProcessEnv = {};
  for (const name of HOST_ENV_NAMES) {
    const value = process.env[name];
    if (value !== undefined) {
      env[name] = value;
    }
  }
  return env;
};

// Wait for close even after a timeout or spawn error so the caller can keep
// its listener open until the child has terminated and its output is drained.
const runDevServer = (args: string[]): Promise<DevServerResult> =>
  new Promise((resolve, reject) => {
    const child = spawn(process.execPath, [rspressBin(), "dev", ...args], {
      cwd: APP_DIRECTORY,
      env: hostEnv(),
      shell: false,
      stdio: ["ignore", "pipe", "pipe"],
    });
    let output = "";
    let timedOut = false;
    let childError: Error | undefined;
    child.stdout.on("data", (chunk: Buffer) => (output += chunk.toString()));
    child.stderr.on("data", (chunk: Buffer) => (output += chunk.toString()));
    const timer = setTimeout(() => {
      timedOut = true;
      child.kill("SIGKILL");
    }, EXIT_TIMEOUT_MS);
    child.once("error", (error) => {
      childError = error;
    });
    child.once("close", (exitCode) => {
      clearTimeout(timer);
      if (timedOut) {
        reject(
          new Error(`rspress dev did not exit within ${EXIT_TIMEOUT_MS} ms`, {
            cause: output,
          }),
        );
        return;
      }
      if (childError) {
        reject(childError);
        return;
      }
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

      expect(result.exitCode).not.toBe(null);
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
      const address = holder.address();
      if (address === null || typeof address !== "object") {
        throw new Error("the port holder has no address");
      }

      const result = await runDevServer([
        "--host",
        LOOPBACK_HOST,
        "--port",
        String(address.port),
      ]);

      expect(result.exitCode).not.toBe(null);
      expect(result.exitCode).not.toBe(0);
      expect(result.output).toContain(`Port ${address.port} is occupied`);
      expect(result.output).not.toContain("Local:");
    } finally {
      await release(holder);
    }
  });
});
