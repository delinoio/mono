// SPDX-License-Identifier: FSL-1.1-ALv2
import { existsSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const launcher = fileURLToPath(new URL("./dev.mjs", import.meta.url));
const [, entry, mode, phase] = process.argv;
const releaseFile = process.env.FIXTURE_RELEASE_GRACE;
const releasePhaseFile = process.env.FIXTURE_RELEASE_PHASE;

if (
  entry === launcher &&
  mode === "--supervise" &&
  phase === "server" &&
  releaseFile
) {
  const setTimeout = globalThis.setTimeout;
  // The supervisor's first timeout schedules escalation. Capture any delay
  // it requests and hold the callback until the test has checked the group.
  globalThis.setTimeout = (callback, delay, ...args) => {
    globalThis.setTimeout = setTimeout;
    const timer = setInterval(() => {
      if (!existsSync(releaseFile)) return;
      clearInterval(timer);
      callback(...args);
    }, 25);
    writeFileSync(
      1,
      JSON.stringify({ event: "grace_timer", delay, pid: process.pid }) + "\n",
    );
    return timer;
  };
}

if (entry === launcher && mode === "--coordinate" && releasePhaseFile) {
  const send = process.send;
  let phaseRequests = 0;
  process.send = function (message, ...args) {
    if (message !== "start-phase" || ++phaseRequests !== 2) {
      return send.call(this, message, ...args);
    }
    // The build supervisor has closed. Hold the coordinator's request to
    // start the server so the test can kill the launcher at this boundary.
    const timer = setInterval(() => {
      if (!existsSync(releasePhaseFile)) return;
      clearInterval(timer);
      if (process.connected) send.call(this, message, ...args);
    }, 25);
    timer.unref();
    writeFileSync(1, JSON.stringify({ event: "phase_boundary" }) + "\n");
    return true;
  };
}
