// SPDX-License-Identifier: Apache-2.0
import { create, type MessageInitShape, toBinary } from "@bufbuild/protobuf";
import { durationFromMs } from "@bufbuild/protobuf/wkt";
import { Code } from "@connectrpc/connect";

import { bindingDigest, execBinding } from "./digest.ts";
import { localError } from "./errors.ts";
import {
  type BaseImage,
  DataPathTransport,
  type NetworkMode,
} from "./gen/delino/ownsand/v1/common_pb.ts";
import { ExecRequestSchema } from "./gen/delino/ownsand/v1/datapath_pb.ts";
import type {
  CreateSandboxRequestSchema,
  Sandbox as SandboxMessage,
  Snapshot,
} from "./gen/delino/ownsand/v1/sandbox_pb.ts";
import { newBindingSalt, newUuidV7 } from "./ids.ts";
import type { RelayContext } from "./relay/context.ts";
import {
  type ExecResult,
  type ExecStreamEvent,
  ExecEventKind,
  relayExec,
} from "./relay/exec.ts";
import { relayReadFile } from "./relay/read.ts";
import { relayWriteFile, type WriteFileInput } from "./relay/write.ts";

/** Longest allowed `maxTtl`: 7 days. */
export const MAX_TTL_LIMIT_MS = 7 * 24 * 60 * 60 * 1000;
/** Largest encoded `command`, `env` and `working_directory` of one exec. */
export const EXEC_REQUEST_FIELDS_LIMIT_BYTES = 256 * 1024;

export type SandboxSourceInput =
  { baseImage: BaseImage } | { snapshotId: string } | { templateId: string };

export interface CreateSandboxOptions {
  source: SandboxSourceInput;
  resources: {
    vcpuCount: number | bigint;
    memoryMib: number | bigint;
    diskGib: number | bigint;
  };
  /** Idle TTL in milliseconds. Required; there is no default. */
  idleTtl: number;
  /** Maximum TTL in milliseconds, at most 7 days. Required; there is no default. */
  maxTtl: number;
  placement?: { deviceId?: string; hostOs?: number; arch?: number };
  network?: NetworkMode;
  /** Never put secrets in labels; every member can read them. */
  labels?: Record<string, string>;
  organizationId?: string;
  signal?: AbortSignal;
}

/** Validates the TTLs and resources of a create before any request is sent. */
export function validateCreate(options: CreateSandboxOptions): void {
  const { idleTtl, maxTtl } = options;
  const ttl = (name: string, v: unknown) => {
    if (typeof v !== "number" || !Number.isFinite(v) || v <= 0) {
      throw localError(
        `${name} is required and must be greater than zero`,
        Code.InvalidArgument,
      );
    }
  };
  ttl("idleTtl", idleTtl);
  ttl("maxTtl", maxTtl);
  if (idleTtl > maxTtl) {
    throw localError(
      "idleTtl must not be greater than maxTtl",
      Code.InvalidArgument,
    );
  }
  if (maxTtl > MAX_TTL_LIMIT_MS) {
    throw localError(
      "maxTtl must not be greater than 7 days",
      Code.InvalidArgument,
    );
  }
  const r = options.resources as
    Partial<CreateSandboxOptions["resources"]> | undefined;
  for (const key of ["vcpuCount", "memoryMib", "diskGib"] as const) {
    const v = r?.[key];
    if (v === undefined || BigInt(v) <= 0n) {
      throw localError(
        `resources.${key} is required and must be greater than zero`,
        Code.InvalidArgument,
      );
    }
  }
  if (!options.source) {
    throw localError("source is required", Code.InvalidArgument);
  }
}

/** Builds the CreateSandboxRequest fields of a create. */
export function createRequestFields(
  options: CreateSandboxOptions,
  idempotencyKey: string,
): MessageInitShape<typeof CreateSandboxRequestSchema> {
  const s = options.source;
  const source =
    "baseImage" in s
      ? { source: { case: "baseImage" as const, value: s.baseImage } }
      : "snapshotId" in s
        ? { source: { case: "snapshotId" as const, value: s.snapshotId } }
        : { source: { case: "templateId" as const, value: s.templateId } };
  return {
    idempotencyKey,
    source,
    resources: {
      vcpuCount: BigInt(options.resources.vcpuCount),
      memoryMib: BigInt(options.resources.memoryMib),
      diskGib: BigInt(options.resources.diskGib),
    },
    idleTtl: durationFromMs(Math.round(options.idleTtl)),
    maxTtl: durationFromMs(Math.round(options.maxTtl)),
    placement: options.placement,
    network:
      options.network === undefined ? undefined : { mode: options.network },
    labels: options.labels ?? {},
  };
}

export interface ExecOptions {
  env?: Record<string, string>;
  workingDirectory?: string;
  /** Exec timeout in milliseconds. The guest helper stops the command when it ends. */
  timeoutMs?: number;
  signal?: AbortSignal;
}

/**
 * One exec. Iterate it for the `started`, output and truncation events, or
 * call `wait()` or `collect()`. A data path failure rejects; it is never an
 * exit code.
 */
export class ExecRun implements AsyncIterable<ExecStreamEvent> {
  readonly #gen: AsyncGenerator<ExecStreamEvent, ExecResult, undefined>;
  readonly #result: Promise<ExecResult>;
  #resolve!: (r: ExecResult) => void;
  #reject!: (e: unknown) => void;
  #iterating = false;

  constructor(
    readonly execId: string,
    gen: AsyncGenerator<ExecStreamEvent, ExecResult, undefined>,
  ) {
    this.#gen = gen;
    this.#result = new Promise<ExecResult>((resolve, reject) => {
      this.#resolve = resolve;
      this.#reject = reject;
    });
    // The result is also reported through iteration; avoid an unhandled rejection.
    this.#result.catch(() => {});
  }

  [Symbol.asyncIterator](): AsyncIterator<ExecStreamEvent> {
    if (this.#iterating) {
      throw new Error("an ExecRun can be iterated only once");
    }
    this.#iterating = true;
    const gen = this.#gen;
    return {
      next: async () => {
        try {
          const r = await gen.next();
          if (r.done) {
            this.#resolve(r.value);
            return { done: true, value: undefined };
          }
          return r;
        } catch (err) {
          this.#reject(err);
          throw err;
        }
      },
      return: async () => {
        await gen.return(undefined as never).catch(() => {});
        this.#reject(
          localError("the exec stream was closed by the caller", Code.Canceled),
        );
        return { done: true, value: undefined };
      },
    };
  }

  /** Waits for the result. Without an iterating caller, it discards the output. */
  async wait(): Promise<ExecResult> {
    if (!this.#iterating) {
      for await (const _ of this) {
        // Drain.
      }
    }
    return this.#result;
  }

  /** Collects stdout and stderr in memory and returns them with the result. */
  async collect(): Promise<{
    result: ExecResult;
    stdout: Uint8Array;
    stderr: Uint8Array;
  }> {
    const out: Uint8Array[] = [];
    const err: Uint8Array[] = [];
    for await (const ev of this) {
      if (ev.kind === ExecEventKind.STDOUT) {
        out.push(ev.data);
      } else if (ev.kind === ExecEventKind.STDERR) {
        err.push(ev.data);
      }
    }
    return {
      result: await this.#result,
      stdout: concat(out),
      stderr: concat(err),
    };
  }
}

/** A file read. Iterate it for chunks or call `bytes()`. */
export class FileRead implements AsyncIterable<Uint8Array> {
  readonly #gen: AsyncGenerator<Uint8Array, bigint, undefined>;

  constructor(gen: AsyncGenerator<Uint8Array, bigint, undefined>) {
    this.#gen = gen;
  }

  [Symbol.asyncIterator](): AsyncIterator<Uint8Array> {
    return this.#gen;
  }

  async bytes(): Promise<Uint8Array> {
    const parts: Uint8Array[] = [];
    for await (const p of this) {
      parts.push(p);
    }
    return concat(parts);
  }
}

function concat(parts: Uint8Array[]): Uint8Array {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.byteLength, 0));
  let o = 0;
  for (const p of parts) {
    out.set(p, o);
    o += p.byteLength;
  }
  return out;
}

function checkGuestPath(path: string): void {
  if (!(
    path.startsWith("/") ||
    /^[A-Za-z]:[\\/]/.test(path) ||
    path.startsWith("\\\\")
  )) {
    throw localError("the guest path must be absolute", Code.InvalidArgument);
  }
}

/** Calls of the control plane that a sandbox handle needs. */
export interface SandboxHandleBackend {
  relay(sandboxId: string, organizationId: string): RelayContext;
  getSandbox(
    organizationId: string,
    sandboxId: string,
    signal?: AbortSignal,
  ): Promise<SandboxMessage>;
  deleteSandbox(
    organizationId: string,
    sandboxId: string,
    signal?: AbortSignal,
  ): Promise<SandboxMessage>;
  pauseSandbox(
    organizationId: string,
    sandboxId: string,
    signal?: AbortSignal,
  ): Promise<SandboxMessage>;
  resumeSandbox(
    organizationId: string,
    sandboxId: string,
    signal?: AbortSignal,
  ): Promise<SandboxMessage>;
  createSnapshot(
    organizationId: string,
    sandboxId: string,
    signal?: AbortSignal,
  ): Promise<Snapshot>;
}

/**
 * A handle to one sandbox. Exec and file operations use the relay data path:
 * the TypeScript SDK is relay-only and never calls `OpenSession`.
 */
export class SandboxHandle {
  readonly #backend: SandboxHandleBackend;

  constructor(
    readonly sandboxId: string,
    readonly organizationId: string,
    backend: SandboxHandleBackend,
  ) {
    this.#backend = backend;
  }

  /** The data path transport in use. Always `RELAY` in the TypeScript SDK. */
  get transport(): DataPathTransport {
    return DataPathTransport.RELAY;
  }

  get(options: { signal?: AbortSignal } = {}): Promise<SandboxMessage> {
    return this.#backend.getSandbox(
      this.organizationId,
      this.sandboxId,
      options.signal,
    );
  }

  delete(options: { signal?: AbortSignal } = {}): Promise<SandboxMessage> {
    return this.#backend.deleteSandbox(
      this.organizationId,
      this.sandboxId,
      options.signal,
    );
  }

  pause(options: { signal?: AbortSignal } = {}): Promise<SandboxMessage> {
    return this.#backend.pauseSandbox(
      this.organizationId,
      this.sandboxId,
      options.signal,
    );
  }

  resume(options: { signal?: AbortSignal } = {}): Promise<SandboxMessage> {
    return this.#backend.resumeSandbox(
      this.organizationId,
      this.sandboxId,
      options.signal,
    );
  }

  snapshot(options: { signal?: AbortSignal } = {}): Promise<Snapshot> {
    return this.#backend.createSnapshot(
      this.organizationId,
      this.sandboxId,
      options.signal,
    );
  }

  /** Runs a command. `command[0]` is the program; no shell runs unless the caller passes one. */
  exec(command: string[], options: ExecOptions = {}): ExecRun {
    if (command.length === 0) {
      throw localError("command must name a program", Code.InvalidArgument);
    }
    if (
      options.timeoutMs !== undefined &&
      !(Number.isFinite(options.timeoutMs) && options.timeoutMs > 0)
    ) {
      throw localError(
        "timeoutMs must be greater than zero",
        Code.InvalidArgument,
      );
    }
    const fields = create(ExecRequestSchema, {
      command,
      env: options.env ?? {},
      workingDirectory: options.workingDirectory ?? "",
    });
    if (
      toBinary(ExecRequestSchema, fields).byteLength >
      EXEC_REQUEST_FIELDS_LIMIT_BYTES
    ) {
      throw localError(
        "the encoded command, env and working directory exceed 256 KiB",
        Code.InvalidArgument,
      );
    }
    const ctx = this.#backend.relay(this.sandboxId, this.organizationId);
    // The exec_id is generated before the request is sent, so the exec can be
    // continued even when the stream ends before `started`.
    const execId = newUuidV7(ctx.clock.wallNow());
    const request = create(ExecRequestSchema, {
      organizationId: this.organizationId,
      sandboxId: this.sandboxId,
      execId,
      command: [...command],
      env: { ...(options.env ?? {}) },
      workingDirectory: options.workingDirectory ?? "",
      timeout:
        options.timeoutMs === undefined
          ? undefined
          : durationFromMs(Math.round(options.timeoutMs)),
      bindingSalt: newBindingSalt(),
    });
    const digest = bindingDigest(execBinding(request));
    return new ExecRun(
      execId,
      relayExec(ctx, execId, request, digest, options.signal ?? neverAborted()),
    );
  }

  /** Attaches to an exec that this caller did not start or whose request it no longer has. */
  attachExec(execId: string, options: { signal?: AbortSignal } = {}): ExecRun {
    const ctx = this.#backend.relay(this.sandboxId, this.organizationId);
    return new ExecRun(
      execId,
      relayExec(
        ctx,
        execId,
        undefined,
        undefined,
        options.signal ?? neverAborted(),
      ),
    );
  }

  /** Streams the contents of an absolute guest path. */
  readFile(path: string, options: { signal?: AbortSignal } = {}): FileRead {
    checkGuestPath(path);
    const ctx = this.#backend.relay(this.sandboxId, this.organizationId);
    return new FileRead(
      relayReadFile(ctx, path, options.signal ?? neverAborted()),
    );
  }

  /** Writes a guest file from a byte buffer, a string or a reader, in chunks. */
  async writeFile(
    path: string,
    data: WriteFileInput,
    options: { mode?: number; signal?: AbortSignal } = {},
  ): Promise<{ sizeBytes: bigint }> {
    checkGuestPath(path);
    const ctx = this.#backend.relay(this.sandboxId, this.organizationId);
    const res = await relayWriteFile(
      ctx,
      path,
      data,
      options.mode ?? 0o644,
      options.signal ?? neverAborted(),
    );
    return { sizeBytes: res.writtenBytes };
  }
}

function neverAborted(): AbortSignal {
  return new AbortController().signal;
}

export { newUuidV7 };
