// SPDX-License-Identifier: Apache-2.0
import {
  create,
  type Message,
  type MessageInitShape,
} from "@bufbuild/protobuf";
import { OwnSandClient, type OwnSandClientOptions } from "../client.ts";
import { SandboxState } from "../gen/delino/ownsand/v1/common_pb.ts";
import { DataPathService } from "../gen/delino/ownsand/v1/data_path_pb.ts";
import type {
  ExecEventSchema,
  FileReadEventSchema,
} from "../gen/delino/ownsand/v1/data_frame_pb.ts";

type ExecEventInit = NonNullable<
  MessageInitShape<typeof ExecEventSchema>["event"]
>;
type FileEventInit = NonNullable<
  MessageInitShape<typeof FileReadEventSchema>["event"]
>;
import {
  AttachExecResponseSchema,
  ExecResponseSchema,
  ReadFileResponseSchema,
} from "../gen/delino/ownsand/v1/relay_pb.ts";
import { SandboxService } from "../gen/delino/ownsand/v1/sandbox_pb.ts";
import { FakeApi } from "./fake-api.ts";
import { VirtualClock } from "./virtual-clock.ts";

export const ORG = "0192f4b6-0000-7000-8000-00000000000a";
export const SANDBOX = "0192f4b6-7c3a-7def-8abc-0123456789ab";
export const API_KEY = "osk_test_0123456789abcdef";

export interface Harness {
  clock: VirtualClock;
  api: FakeApi;
  client: OwnSandClient;
  /** Grants issued by the fake AuthorizeRelay, in order. */
  grants: string[];
  /** Current sandbox state for the fake GetSandbox. */
  state: { value: SandboxState };
}

/** A relay-only client against an in-memory control plane. */
export function setup(
  options:
    | Partial<OwnSandClientOptions>
    | ((clock: VirtualClock) => Partial<OwnSandClientOptions>) = {},
): Harness {
  const clock = new VirtualClock();
  const extra = typeof options === "function" ? options(clock) : options;
  const api = new FakeApi(clock);
  const grants: string[] = [];
  const state = { value: SandboxState.RUNNING };
  const byAuthorization = new Map<string, string>();
  api.unary(DataPathService, "AuthorizeRelay", (req) => {
    // A retry with the same authorization_id returns the same grant.
    let grant = byAuthorization.get(req.authorizationId);
    if (grant === undefined) {
      grant = `grant-${grants.length + 1}`;
      grants.push(grant);
      byAuthorization.set(req.authorizationId, grant);
    }
    return {
      grantId: grant,
      expiresIn: { seconds: 900n },
      expireTime: { seconds: 0n },
    };
  });
  api.unary(SandboxService, "GetSandbox", (req) => ({
    sandbox: { sandboxId: req.sandboxId, state: state.value },
  }));
  const client = new OwnSandClient({
    origin: api.origin,
    apiKey: API_KEY,
    organizationId: ORG,
    fetch: api.fetch,
    clock,
    webrtc: false,
    env: {},
    ...extra,
  });
  return { clock, api, client, grants, state };
}

export const ev = {
  accepted: (): ExecEventInit => ({ case: "accepted", value: {} }),
  keepalive: (): ExecEventInit => ({ case: "keepalive", value: {} }),
  started: (execId = ""): ExecEventInit => ({
    case: "started",
    value: { execId },
  }),
  stdout: (offset: number, text: string): ExecEventInit => ({
    case: "stdout",
    value: { offset: BigInt(offset), data: new TextEncoder().encode(text) },
  }),
  stderr: (offset: number, text: string): ExecEventInit => ({
    case: "stderr",
    value: { offset: BigInt(offset), data: new TextEncoder().encode(text) },
  }),
  truncated: (stdout: number, stderr: number): ExecEventInit => ({
    case: "outputTruncated",
    value: { stdoutOffset: BigInt(stdout), stderrOffset: BigInt(stderr) },
  }),
  exited: (code: number, timedOut = false): ExecEventInit => ({
    case: "exited",
    value: { status: { case: "exitCode", value: BigInt(code) }, timedOut },
  }),
  paused: (): ExecEventInit => ({ case: "paused", value: {} }),
};

export function execMsg(event: ExecEventInit): Message {
  return create(ExecResponseSchema, { event: { event } });
}

export function attachMsg(event: ExecEventInit): Message {
  return create(AttachExecResponseSchema, { event: { event } });
}

export const file = {
  accepted: (): Message => readMsg({ case: "accepted", value: {} }),
  keepalive: (): Message => readMsg({ case: "keepalive", value: {} }),
  chunk: (offset: number, data: Uint8Array | string): Message =>
    readMsg({
      case: "chunk",
      value: {
        offset: BigInt(offset),
        data: typeof data === "string" ? new TextEncoder().encode(data) : data,
      },
    }),
  end: (size: number): Message =>
    readMsg({ case: "end", value: { sizeBytes: BigInt(size) } }),
};

function readMsg(event: FileEventInit): Message {
  return create(ReadFileResponseSchema, { event: { event } });
}

export const text = (b: Uint8Array) => new TextDecoder().decode(b);
