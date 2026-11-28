// SPDX-License-Identifier: Apache-2.0
//
// POSIX fixture commands run in a group owned by a dedicated Node process.
import { fork, spawn } from "node:child_process";
import { closeSync } from "node:fs";
import { finished } from "node:stream/promises";
import { StringDecoder } from "node:string_decoder";
import { fileURLToPath } from "node:url";

function ownProcessGroup() {
  let command;
  const report = (message) => {
    if (process.connected) process.send(message, () => {});
  };
  const stop = () => process.kill(0, "SIGKILL");

  // A terminal-style signal must reach the command and its descendants while
  // the owner stays in the group to handle teardown.
  for (const signal of ["SIGHUP", "SIGINT", "SIGTERM"])
    process.on(signal, () => {});

  process.once("disconnect", stop);
  process.on("message", (message) => {
    switch (message.type) {
      case "start": {
        const reportError = (error) =>
          report({
            type: "commandError",
            message: error.message,
            details: { ...error },
          });
        try {
          command = spawn(message.command, message.args, {
            ...message.options,
            stdio: "inherit",
          });
          command.once("spawn", () =>
            report({ type: "started", pid: command.pid }),
          );
          command.once("error", reportError);
          command.once("exit", (status, signal) =>
            report({ type: "commandExit", status, signal }),
          );
        } catch (error) {
          reportError(error);
        } finally {
          // Only the command and its descendants should hold the fixture
          // pipes open. They inherited the descriptors before we close ours.
          for (const fd of [0, 1, 2]) closeSync(fd);
        }
        break;
      }
      case "signalCommand":
        command?.kill(message.signal);
        break;
      case "signalGroup":
        process.kill(0, message.signal);
        break;
      case "stop":
        // Report the stop before SIGKILL removes the owner too.
        process.send({ type: "stopping" }, stop);
        break;
    }
  });
}

async function capture(stream) {
  const decoder = new StringDecoder("utf8");
  let output = "";
  stream.on("data", (chunk) => (output += decoder.write(chunk)));
  await finished(stream, { cleanup: true });
  return output + decoder.end();
}

// The command PID and direct signal target belong to the command. Group
// signals go through the owner, including after the command has exited.
export function startProcessGroup(command, args, { cwd, env } = {}) {
  const owner = fork(fileURLToPath(import.meta.url), [], {
    detached: true,
    execArgv: [],
    stdio: ["pipe", "pipe", "pipe", "ipc"],
  });
  const send = (message) => {
    if (owner.connected) owner.send(message, () => {});
  };
  const child = {
    pid: undefined,
    exitCode: null,
    signalCode: null,
    stdin: owner.stdin,
    stdout: owner.stdout,
    stderr: owner.stderr,
    kill: (signal = "SIGTERM") => send({ type: "signalCommand", signal }),
  };
  const commandExit = Promise.withResolvers();
  const ownerClose = Promise.withResolvers();
  const ownerFailure = Promise.withResolvers();
  let stopping = false;
  let ownerError;

  function recordExit(status, signal) {
    child.exitCode = status;
    child.signalCode = signal;
    commandExit.resolve(status);
  }

  function loseOwner(error) {
    if (ownerError) return;
    ownerError = error;
    ownerFailure.reject(error);
    child.stdin.destroy();
    child.stdout.destroy();
    child.stderr.destroy();
  }

  const commandDone = Promise.all([
    commandExit.promise,
    capture(child.stdout),
    capture(child.stderr),
  ]).then(([status, stdout, stderr]) => ({ status, stdout, stderr }));
  const done = Promise.race([commandDone, ownerFailure.promise]);
  // A caller can fail before it reaches its await of done.
  done.catch(() => {});
  child.stdin.on("error", (error) => {
    if (error.code !== "EPIPE" && error.code !== "ECONNRESET")
      commandExit.reject(error);
  });

  let commandExited = false;
  owner.on("message", (message) => {
    switch (message.type) {
      case "started":
        child.pid = message.pid;
        break;
      case "commandError":
        commandExit.reject(
          Object.assign(new Error(message.message), message.details),
        );
        break;
      case "commandExit":
        commandExited = true;
        recordExit(message.status, message.signal);
        break;
      case "stopping":
        stopping = true;
        break;
    }
  });
  owner.once("error", loseOwner);
  owner.once("disconnect", () => {
    // The owner can disappear while a descendant still holds a pipe.
    if (!stopping) loseOwner(new Error("Fixture process owner disconnected"));
  });
  owner.once("close", (status, signal) => {
    if (stopping && signal === "SIGKILL") {
      if (!commandExited) recordExit(null, signal);
    } else if (!ownerError) {
      loseOwner(
        new Error(`Fixture process owner exited with ${signal ?? status}`),
      );
    }
    ownerClose.resolve();
  });

  send({
    type: "start",
    command,
    args,
    options: { cwd: cwd instanceof URL ? fileURLToPath(cwd) : cwd, env },
  });

  let reaping;
  const reap = () =>
    (reaping ??= (async () => {
      send({ type: "stop" });
      await ownerClose.promise;
      if (ownerError) throw ownerError;
      return done;
    })());
  return {
    child,
    done,
    reap,
    signalGroup: (signal) => send({ type: "signalGroup", signal }),
  };
}

export function spawnProcessGroup(t, command, args, options) {
  const run = startProcessGroup(command, args, options);
  t.after(run.reap);
  return run;
}

if (import.meta.main && process.send) ownProcessGroup();
