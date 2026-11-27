// SPDX-License-Identifier: Apache-2.0
import assert from "node:assert/strict";
import { Role, SandRunClient, WatchQueueEventKind } from "sandrun-sdk";

assert.equal(typeof SandRunClient, "function");
assert.equal(WatchQueueEventKind.Request, "request");
assert.equal(Role.OWNER, 1);
