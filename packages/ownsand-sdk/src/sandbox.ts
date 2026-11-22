// SPDX-License-Identifier: Apache-2.0
import { create } from "@bufbuild/protobuf";
import { DataPathSelector } from "./datapath.ts";
import { newBindingSalt } from "./digest.ts";
import {
  DataPathTransport,
  SandboxState,
} from "@delino/ownsand-protos/v1/common_pb";
import { ExecRequestSchema } from "@delino/ownsand-protos/v1/data_frame_pb";
import type { Sandbox as SandboxMessage } from "@delino/ownsand-protos/v1/sandbox_pb";
import type { Snapshot } from "@delino/ownsand-protos/v1/snapshot_pb";
import type { Client } from "@connectrpc/connect";
import type { SnapshotService } from "@delino/ownsand-protos/v1/snapshot_pb";
import { ConsumerHold } from "./relay/stream.ts";
import {
  ExecHandle,
  type ExecResult,
  ExecEventType,
  runExec,
} from "./operations/exec.ts";
import { runReadFile } from "./operations/read.ts";
import {
  DEFAULT_FILE_MODE,
  runWriteFile,
  type WriteFileResult,
  type WriteSource,
} from "./operations/write.ts";
import type { PeerConnectionConstructor } from "./p2p/webrtc.ts";
import type { OperationContext } from "./relay/context.ts";
import {
  durationFromMs,
  validateExec,
  validateFileMode,
  validateGuestPath,
} from "./validate.ts";
import { OwnSandError } from "./errors.ts";

/** Options of one exec. */
export interface ExecOptions {
  /** Argument array; the first element is the program. No shell runs unless passed. */
  command: readonly string[];
  env?: Readonly<Record<string, string>>;
  workingDirectory?: string;
  /** The guest helper stops the command after this many milliseconds. */
  timeoutMs?: number;
  signal?: AbortSignal;
}

/** Output and result of `run`. */
export interface RunResult {
  stdout: Uint8Array;
  stderr: Uint8Array;
  result: ExecResult;
}

/** States after which a sandbox does not change by itself. */
const STABLE_STATES: ReadonlySet<SandboxState> = new Set([
  SandboxState.RUNNING,
  SandboxState.PAUSED,
  SandboxState.DELETED,
  SandboxState.FAILED,
]);

/**
 * A handle of one sandbox. It opens at most one peer-to-peer session, only
 * while an operation is open, and uses the relay otherwise.
 */
export class Sandbox {
  readonly sandboxId: string;
  readonly organizationId: string;
  /** The sandbox as the last RPC of this handle returned it. */
  info: SandboxMessage | undefined;
  readonly #ctx: OperationContext;
  readonly #snapshots: Client<typeof SnapshotService>;
  readonly #selector: DataPathSelector;

  constructor(
    ctx: OperationContext,
    snapshots: Client<typeof SnapshotService>,
    Peer: PeerConnectionConstructor | undefined,
    forced: DataPathTransport | undefined,
    info: SandboxMessage | undefined,
  ) {
    this.#ctx = ctx;
    this.#snapshots = snapshots;
    this.sandboxId = ctx.sandboxId;
    this.organizationId = ctx.organizationId;
    this.info = info;
    this.#selector = new DataPathSelector(ctx, Peer, forced);
  }

  /** The data path transport in use. */
  get transport(): DataPathTransport {
    return this.#selector.transport;
  }

  #ids() {
    return { organizationId: this.organizationId, sandboxId: this.sandboxId };
  }

  async get(signal?: AbortSignal): Promise<SandboxMessage> {
    const res = await this.#ctx.sandboxes.getSandbox(this.#ids(), { signal });
    return this.#keep(res.sandbox);
  }

  async delete(signal?: AbortSignal): Promise<SandboxMessage> {
    const res = await this.#ctx.sandboxes.deleteSandbox(this.#ids(), {
      signal,
    });
    // Close only after the delete succeeded: a failed delete leaves the
    // sandbox and its handle usable.
    this.#selector.close();
    return this.#keep(res.sandbox);
  }

  async pause(signal?: AbortSignal): Promise<SandboxMessage> {
    const res = await this.#ctx.sandboxes.pauseSandbox(this.#ids(), {
      signal,
    });
    return this.#keep(res.sandbox);
  }

  async resume(signal?: AbortSignal): Promise<SandboxMessage> {
    const res = await this.#ctx.sandboxes.resumeSandbox(this.#ids(), {
      signal,
    });
    return this.#keep(res.sandbox);
  }

  /** Takes a snapshot. It returns at once with the snapshot in `CREATING`. */
  async snapshot(signal?: AbortSignal): Promise<Snapshot> {
    const res = await this.#snapshots.createSnapshot(this.#ids(), { signal });
    if (res.snapshot === undefined) {
      throw new OwnSandError("CreateSnapshot returned no snapshot");
    }
    return res.snapshot;
  }

  /**
   * Polls `GetSandbox` until the sandbox is `RUNNING`, `PAUSED`, `DELETED`
   * or `FAILED`.
   */
  async waitUntilStable(
    options: { pollMs?: number; signal?: AbortSignal } = {},
  ): Promise<SandboxMessage> {
    for (;;) {
      const sandbox = await this.get(options.signal);
      if (STABLE_STATES.has(sandbox.state)) {
        return sandbox;
      }
      await this.#ctx.clock.sleep(options.pollMs ?? 1000, options.signal);
    }
  }

  /** Starts an exec. Iterate the handle for output; `wait()` returns the result. */
  exec(options: ExecOptions): ExecHandle {
    validateExec(options);
    const execId = this.#ctx.ids.next();
    const request = create(ExecRequestSchema, {
      organizationId: this.organizationId,
      sandboxId: this.sandboxId,
      execId,
      command: [...options.command],
      env: { ...options.env },
      workingDirectory: options.workingDirectory ?? "",
      timeout:
        options.timeoutMs === undefined
          ? undefined
          : durationFromMs(options.timeoutMs),
      bindingSalt: newBindingSalt(),
    });
    const hold = new ConsumerHold();
    return new ExecHandle(
      execId,
      runExec(
        this.#ctx,
        this.#selector,
        { execId, request, stdoutOffset: 0n, stderrOffset: 0n },
        options.signal,
        hold,
      ),
      this.#selector,
      hold,
    );
  }

  /**
   * Attaches to an exec that this handle did not start, with an
   * `ATTACH_EXEC` grant that carries only the exec ID.
   */
  attachExec(
    execId: string,
    options: {
      stdoutOffset?: bigint;
      stderrOffset?: bigint;
      signal?: AbortSignal;
    } = {},
  ): ExecHandle {
    const hold = new ConsumerHold();
    return new ExecHandle(
      execId,
      runExec(
        this.#ctx,
        this.#selector,
        {
          execId,
          request: undefined,
          stdoutOffset: options.stdoutOffset ?? 0n,
          stderrOffset: options.stderrOffset ?? 0n,
        },
        options.signal,
        hold,
      ),
      this.#selector,
      hold,
    );
  }

  /** Runs a command and collects its output. */
  async run(options: ExecOptions): Promise<RunResult> {
    const handle = this.exec(options);
    const stdout: Uint8Array[] = [];
    const stderr: Uint8Array[] = [];
    for await (const event of handle) {
      if (event.type === ExecEventType.Stdout) stdout.push(event.data);
      if (event.type === ExecEventType.Stderr) stderr.push(event.data);
    }
    return {
      stdout: concat(stdout),
      stderr: concat(stderr),
      result: await handle.wait(),
    };
  }

  /** Streams a guest file in chunks, from `offset`. */
  readFile(
    path: string,
    options: { offset?: bigint; signal?: AbortSignal } = {},
  ): AsyncIterable<Uint8Array> {
    validateGuestPath("path", path);
    const ctx = this.#ctx;
    const selector = this.#selector;
    return {
      [Symbol.asyncIterator]: () =>
        runReadFile(ctx, selector, path, options.offset ?? 0n, options.signal),
    };
  }

  /** Reads a whole guest file into memory. */
  async readFileBytes(
    path: string,
    options: { signal?: AbortSignal } = {},
  ): Promise<Uint8Array> {
    const chunks: Uint8Array[] = [];
    for await (const chunk of this.readFile(path, options)) {
      chunks.push(chunk);
    }
    return concat(chunks);
  }

  /** Writes a guest file from a buffer, string, iterable or stream. */
  async writeFile(
    path: string,
    source: WriteSource,
    options: { mode?: number; signal?: AbortSignal } = {},
  ): Promise<WriteFileResult> {
    validateGuestPath("path", path);
    const mode = options.mode ?? DEFAULT_FILE_MODE;
    validateFileMode(mode);
    return runWriteFile(
      this.#ctx,
      this.#selector,
      path,
      source,
      mode,
      options.signal,
    );
  }

  /** Closes the peer-to-peer session of the handle, if any. */
  close(): void {
    this.#selector.close();
  }

  #keep(sandbox: SandboxMessage | undefined): SandboxMessage {
    if (sandbox === undefined) {
      throw new OwnSandError("the response carried no sandbox");
    }
    this.info = sandbox;
    return sandbox;
  }
}

function concat(chunks: readonly Uint8Array[]): Uint8Array {
  const total = chunks.reduce((n, c) => n + c.length, 0);
  const out = new Uint8Array(total);
  let at = 0;
  for (const chunk of chunks) {
    out.set(chunk, at);
    at += chunk.length;
  }
  return out;
}
