// SPDX-License-Identifier: Apache-2.0
// An in-process fake of the OwnSand control plane, relay and device agent.
// It implements the relay rules that the SDK depends on (grants, admission,
// replacement, idle release, rotation, keepalives, digests, exec records,
// read snapshots and write ranges) on a virtual clock, plus hooks that inject
// failures into single calls.
import { create } from "@bufbuild/protobuf";
import { durationFromMs, timestampFromMs } from "@bufbuild/protobuf/wkt";
import {
  Code,
  ConnectError,
  type ConnectRouter,
  type HandlerContext,
} from "@connectrpc/connect";

import {
  bindingDigest,
  execBinding,
  readBinding,
  writeBinding,
} from "../../src/digest.ts";
import {
  RelayOperationKind,
  RetryDetailSchema,
  SandboxEndReason,
  SandboxState,
  SessionEndDetailSchema,
  SessionEndReason,
} from "../../src/gen/delino/ownsand/v1/common_pb.ts";
import {
  type AttachExecRequest,
  type AuthorizeRelayRequest,
  DataPathService,
  type ExecEvent,
  ExecEventSchema,
  type ExecRequest,
  OutputStream,
  type ReadFileRequest,
  type ReadFileResponse,
  ReadFileResponseSchema,
  RelayService,
  type WriteFileRequest,
} from "../../src/gen/delino/ownsand/v1/datapath_pb.ts";
import { OrganizationService } from "../../src/gen/delino/ownsand/v1/organization_pb.ts";
import {
  SandboxSchema,
  SandboxService,
  SnapshotService,
} from "../../src/gen/delino/ownsand/v1/sandbox_pb.ts";
import type { Clock } from "../../src/clock.ts";
import { DISCONNECT_HEADER, DRIP_HEADER } from "./faulty-client.ts";

export const ORG = "0190f5d6-0000-7000-8000-00000000000a";
export const SANDBOX = "0190f5d6-0000-7000-8000-00000000000b";
export const API_KEY = "osk_live_test_secret_value";

const MIN = 60_000;
const GRANT_TTL_MS = 15 * MIN;
const STREAM_DEADLINE_MS = 30 * MIN;
const ROTATION_MS = 14 * MIN - 1_000;
const KEEPALIVE_MS = 30_000;
const IDLE_RELEASE_MS = 60_000;

/** An error that the test HTTP client turns into a cut connection. */
export function disconnect(): ConnectError {
  return new ConnectError("fake disconnect", Code.Aborted, {
    [DISCONNECT_HEADER]: "1",
  });
}

export function sessionEnd(reason: SessionEndReason): ConnectError {
  return new ConnectError("relay session ended", Code.Unavailable, undefined, [
    { desc: SessionEndDetailSchema, value: { reason } },
  ]);
}

export function exhausted(retryMs?: number): ConnectError {
  return new ConnectError(
    "at capacity",
    Code.ResourceExhausted,
    undefined,
    retryMs === undefined
      ? undefined
      : [
          {
            desc: RetryDetailSchema,
            value: { retryDelay: durationFromMs(retryMs) },
          },
        ],
  );
}

interface FakeGrant {
  id: string;
  sandboxId: string;
  kind: RelayOperationKind;
  operationId: string;
  digest: Uint8Array;
  createdAt: number;
  expiresAt: number;
  idleSince: number;
  openStreams: number;
  released: boolean;
  revoked: boolean;
  replacedBy?: string;
}

export interface ProgramStep {
  delayMs: number;
  stdout?: string;
  stderr?: string;
  exit?: number;
  signal?: number;
  failed?: string;
}

interface Output {
  stream: OutputStream;
  offset: bigint;
  data: Uint8Array;
}

interface FakeExec {
  execId: string;
  digest: Uint8Array;
  runs: number;
  outputs: Output[];
  stdoutLen: bigint;
  stderrLen: bigint;
  /** First kept offsets, to simulate the 8 MiB retention limit. */
  keptStdout: bigint;
  keptStderr: bigint;
  final?: ExecEvent["event"];
  waiters: Set<() => void>;
}

/** What a hook asks the fake to do with one relay stream. */
export interface StreamPlan {
  /** Fail the RPC before the agent sees it. */
  reject?: ConnectError;
  /** Wait this long before `accepted`, sending relay keepalives every 30 s when `keepaliveBeforeAccept`. */
  acceptDelayMs?: number;
  keepaliveBeforeAccept?: boolean;
  /** End with OK before `accepted` (an end that is not a rotation). */
  endBeforeAccept?: boolean;
  /** Cut the connection before `accepted`. */
  disconnectBeforeAccept?: boolean;
  /** Cut the connection this long after `accepted`. */
  disconnectAfterMs?: number;
  /** Cut the connection after this many data messages (output chunks or file chunks). */
  disconnectAfterData?: number;
  /** After `accepted`, send nothing at all (no keepalives) until cancelled. */
  stallAfterAccept?: boolean;
  /** Deliver large messages in `pieces` pieces `intervalMs` apart. */
  drip?: { pieces: number; intervalMs: number };
}

export interface WritePlan {
  reject?: ConnectError;
  /** Apply the chunk, then cut the connection (lost response). */
  disconnectAfterApply?: boolean;
  /** Never answer until cancelled. */
  hang?: boolean;
  /** Wait this long, then fail with `deadline_exceeded`. */
  deadlineAfterMs?: number;
}

export interface AuthorizePlan {
  reject?: ConnectError;
  /** Install the grant, then cut the connection (lost response). */
  loseResponse?: boolean;
}

interface CallRecord {
  rpc: string;
  at: number;
  grantId?: string;
  authorizationId?: string;
  replaces?: string;
  kind?: RelayOperationKind;
  operationId?: string;
  digest?: Uint8Array;
  stdoutOffset?: bigint;
  stderrOffset?: bigint;
  offset?: bigint;
  writeOffset?: bigint;
  finish?: boolean;
  salt?: Uint8Array;
  path?: string;
}

/** The fake's time source: the SDK clock plus the server's own wall clock. */
export type FakeClock = Clock & { serverWallNow(): number };

export class FakeOwnSand {
  readonly clock: FakeClock;
  readonly calls: CallRecord[] = [];
  readonly violations: string[] = [];
  readonly grants = new Map<string, FakeGrant>();
  readonly authorizations = new Map<string, string>();
  readonly execs = new Map<string, FakeExec>();
  readonly files = new Map<string, Uint8Array>();
  readonly reads = new Map<string, { data: Uint8Array; digest: Uint8Array }>();
  readonly writes = new Map<
    string,
    {
      digest: Uint8Array;
      path: string;
      buf: Uint8Array;
      written: boolean[];
      finished: boolean;
      size?: bigint;
    }
  >();
  readonly programs = new Map<string, ProgramStep[]>();
  readonly createKeys = new Map<string, string>();
  sandboxState = SandboxState.RUNNING;
  sandboxEndReason = SandboxEndReason.UNSPECIFIED;
  credentialRevoked = false;
  /** Per-device grant limit. */
  grantLimit = 256;
  /** AuthorizeRelay calls per second per principal. */
  authorizeRate = 10;
  /** Bytes per ReadFile chunk. */
  readChunkBytes = 64 * 1024;
  openSessionCalls = 0;
  hooks: {
    authorize?: (
      req: AuthorizeRelayRequest,
      n: number,
    ) => AuthorizePlan | undefined;
    stream?: (
      rpc: "Exec" | "AttachExec" | "ReadFile",
      n: number,
    ) => StreamPlan | undefined;
    write?: (req: WriteFileRequest, n: number) => WritePlan | undefined;
    createSandbox?: (n: number) => "lose" | undefined;
  } = {};
  #seq = 0;
  #streamCount = 0;
  #authorizeCount = 0;
  #writeCount = 0;
  #createCount = 0;
  #pauseWaiters = new Set<() => void>();

  constructor(clock: FakeClock) {
    this.clock = clock;
  }

  /** Fake IDs: unique, ordered and readable. */
  #id(prefix: string): string {
    return `${prefix}-${String(++this.#seq).padStart(4, "0")}`;
  }

  callsOf(rpc: string): CallRecord[] {
    return this.calls.filter((c) => c.rpc === rpc);
  }

  setSandboxState(
    state: SandboxState,
    endReason = SandboxEndReason.UNSPECIFIED,
  ): void {
    this.sandboxState = state;
    this.sandboxEndReason = endReason;
    if (state !== SandboxState.PAUSED) {
      for (const w of this.#pauseWaiters) {
        w();
      }
      this.#pauseWaiters.clear();
    }
    for (const e of this.execs.values()) {
      this.#notify(e);
    }
  }

  /** Drops kept output older than the given offsets (the agent's retention limit). */
  truncateExec(execId: string, keptStdout: bigint, keptStderr = 0n): void {
    const e = this.execs.get(execId)!;
    e.keptStdout = keptStdout;
    e.keptStderr = keptStderr;
  }

  /** Every exec_id that the fake agent ran more than once. */
  execsRunTwice(): string[] {
    return [...this.execs.values()]
      .filter((e) => e.runs > 1)
      .map((e) => e.execId);
  }

  // ------------------------------------------------------------------ auth

  #checkAuth(ctx: HandlerContext, rpc: string): void {
    if (
      ctx.requestHeader.get("authorization") !== `Bearer ${API_KEY}` ||
      this.credentialRevoked
    ) {
      throw new ConnectError(
        `${rpc}: invalid credential`,
        Code.Unauthenticated,
      );
    }
  }

  // ---------------------------------------------------------------- grants

  #revocable(g: FakeGrant): boolean {
    const now = this.clock.now();
    this.#maybeRelease(g);
    return (
      !g.released &&
      !g.revoked &&
      now <= g.createdAt + STREAM_DEADLINE_MS + 60_000
    );
  }

  #maybeRelease(g: FakeGrant): void {
    const now = this.clock.now();
    if (
      g.openStreams === 0 &&
      (now - g.idleSince > IDLE_RELEASE_MS || now > g.expiresAt + 30_000)
    ) {
      g.released = true;
    }
  }

  #admit(
    ctx: HandlerContext,
    kinds: RelayOperationKind[],
    operationId: string,
  ): FakeGrant {
    const id = ctx.requestHeader.get("ownsand-relay-grant-id") ?? "";
    const g = this.grants.get(id);
    if (!g) {
      throw sessionEnd(SessionEndReason.GRANT_INVALID);
    }
    this.#maybeRelease(g);
    if (g.released || g.revoked || this.clock.now() > g.expiresAt + 30_000) {
      throw sessionEnd(SessionEndReason.GRANT_INVALID);
    }
    if (
      !kinds.includes(g.kind) ||
      g.operationId !== operationId ||
      g.sandboxId !== SANDBOX
    ) {
      throw new ConnectError(
        "request does not match the grant",
        Code.PermissionDenied,
      );
    }
    return g;
  }

  /** Marks a stream of a grant open until `close` or the caller's cancellation. */
  #track(g: FakeGrant, ctx: HandlerContext): () => void {
    g.openStreams++;
    let closed = false;
    const close = () => {
      if (!closed) {
        closed = true;
        g.openStreams--;
        g.idleSince = this.clock.now();
      }
    };
    ctx.signal.addEventListener("abort", close, { once: true });
    return close;
  }

  authorizeRelay = async (req: AuthorizeRelayRequest, ctx: HandlerContext) => {
    this.#checkAuth(ctx, "AuthorizeRelay");
    const n = this.#authorizeCount++;
    const now = this.clock.now();
    this.calls.push({
      rpc: "AuthorizeRelay",
      at: now,
      authorizationId: req.authorizationId,
      replaces: req.replacesGrantId,
      kind: req.operationKind,
      operationId: req.operationId,
      digest: req.requestDigest,
    });
    const plan = this.hooks.authorize?.(req, n);
    if (plan?.reject) {
      throw plan.reject;
    }
    if (this.sandboxState === SandboxState.UNREACHABLE) {
      throw new ConnectError("sandbox unreachable", Code.Unavailable);
    }
    const running = this.sandboxState === SandboxState.RUNNING;
    const pausedOk =
      this.sandboxState === SandboxState.PAUSED &&
      (req.operationKind === RelayOperationKind.EXEC ||
        req.operationKind === RelayOperationKind.ATTACH_EXEC);
    if (!running && !pausedOk) {
      throw new ConnectError("sandbox is not running", Code.FailedPrecondition);
    }
    const recent = this.callsOf("AuthorizeRelay").filter(
      (c) => now - c.at < 1_000,
    ).length;
    if (recent > this.authorizeRate) {
      throw exhausted(1_000);
    }
    const known = this.authorizations.get(req.authorizationId);
    if (known) {
      const g = this.grants.get(known)!;
      return this.#grantResponse(g, plan);
    }
    let replaced: FakeGrant | undefined;
    if (req.replacesGrantId !== undefined) {
      const old = this.grants.get(req.replacesGrantId);
      if (
        old &&
        this.#revocable(old) &&
        old.kind === req.operationKind &&
        old.operationId === req.operationId &&
        sameBytes(old.digest, req.requestDigest)
      ) {
        replaced = old;
      }
    }
    if (!replaced) {
      const live = [...this.grants.values()].filter((g) =>
        this.#revocable(g),
      ).length;
      if (live >= this.grantLimit) {
        throw exhausted(2_000);
      }
    } else {
      if (replaced.openStreams > 0) {
        this.violations.push(
          `grant ${replaced.id} renewed while ${replaced.openStreams} stream(s) were open`,
        );
      }
      replaced.released = true;
    }
    const g: FakeGrant = {
      id: this.#id("grant"),
      sandboxId: req.sandboxId,
      kind: req.operationKind,
      operationId: req.operationId,
      digest: req.requestDigest,
      createdAt: now,
      expiresAt: now + GRANT_TTL_MS,
      idleSince: now,
      openStreams: 0,
      released: false,
      revoked: false,
    };
    if (replaced) {
      replaced.replacedBy = g.id;
    }
    this.grants.set(g.id, g);
    this.authorizations.set(req.authorizationId, g.id);
    return this.#grantResponse(g, plan);
  };

  #grantResponse(g: FakeGrant, plan: AuthorizePlan | undefined) {
    if (plan?.loseResponse) {
      throw disconnect();
    }
    const remaining = Math.round(g.expiresAt - this.clock.now());
    return {
      grantId: g.id,
      // The server's wall clock; the caller's clock may differ.
      expiresAt: timestampFromMs(
        Math.round(this.clock.serverWallNow()) + remaining,
      ),
      expiresIn: durationFromMs(remaining),
    };
  }

  // ------------------------------------------------------------------ exec

  #notify(e: FakeExec): void {
    for (const w of e.waiters) {
      w();
    }
    e.waiters.clear();
  }

  async #untilRunning(): Promise<void> {
    while (this.sandboxState === SandboxState.PAUSED) {
      await new Promise<void>((resolve) => this.#pauseWaiters.add(resolve));
    }
  }

  #startExec(req: ExecRequest, digest: Uint8Array): FakeExec {
    const e: FakeExec = {
      execId: req.execId,
      digest,
      runs: 1,
      outputs: [],
      stdoutLen: 0n,
      stderrLen: 0n,
      keptStdout: 0n,
      keptStderr: 0n,
      waiters: new Set(),
    };
    this.execs.set(req.execId, e);
    const steps = this.programs.get(req.command.join(" ")) ?? [
      { delayMs: 0, exit: 0 },
    ];
    void (async () => {
      for (const step of steps) {
        await this.clock.sleep(step.delayMs);
        await this.#untilRunning();
        if (
          this.sandboxState === SandboxState.DELETED ||
          this.sandboxState === SandboxState.FAILED
        ) {
          e.final = {
            case: "sandboxEnded",
            value: {
              $typeName: "delino.ownsand.v1.ExecEvent.SandboxEnded",
              reason: this.sandboxEndReason,
            },
          };
          this.#notify(e);
          return;
        }
        for (const [text, stream] of [
          [step.stdout, OutputStream.STDOUT],
          [step.stderr, OutputStream.STDERR],
        ] as const) {
          if (text !== undefined) {
            const data = new TextEncoder().encode(text);
            const isErr = stream === OutputStream.STDERR;
            const offset = isErr ? e.stderrLen : e.stdoutLen;
            e.outputs.push({ stream, offset, data });
            if (isErr) {
              e.stderrLen += BigInt(data.byteLength);
            } else {
              e.stdoutLen += BigInt(data.byteLength);
            }
          }
        }
        if (step.failed !== undefined) {
          e.final = {
            case: "failed",
            value: {
              $typeName: "delino.ownsand.v1.ExecEvent.Failed",
              reason: step.failed,
            },
          };
        } else if (step.exit !== undefined || step.signal !== undefined) {
          e.final = {
            case: "exited",
            value: {
              $typeName: "delino.ownsand.v1.ExecEvent.Exited",
              result:
                step.signal !== undefined
                  ? { case: "signal", value: step.signal }
                  : { case: "exitCode", value: BigInt(step.exit!) },
              timedOut: false,
            },
          };
        }
        this.#notify(e);
        if (e.final) {
          return;
        }
      }
    })();
    return e;
  }

  #applyPlanHeaders(plan: StreamPlan | undefined, ctx: HandlerContext): void {
    if (plan?.drip) {
      ctx.responseHeader.set(
        DRIP_HEADER,
        `${plan.drip.pieces}:${plan.drip.intervalMs}`,
      );
    }
  }

  /** Waits before `accepted`, with optional relay keepalives. */
  async *#beforeAccept<T>(
    plan: StreamPlan | undefined,
    ctx: HandlerContext,
    keepalive: () => T,
  ): AsyncGenerator<T, boolean> {
    let wait = plan?.acceptDelayMs ?? 0;
    while (wait > 0) {
      const step = plan?.keepaliveBeforeAccept
        ? Math.min(wait, KEEPALIVE_MS)
        : wait;
      await this.clock.sleep(step, ctx.signal);
      wait -= step;
      if (plan?.keepaliveBeforeAccept) {
        yield keepalive();
      }
    }
    if (plan?.disconnectBeforeAccept) {
      throw disconnect();
    }
    return !plan?.endBeforeAccept;
  }

  exec = (req: ExecRequest, ctx: HandlerContext) =>
    this.#execStream("Exec", req, ctx);

  attachExec = (req: AttachExecRequest, ctx: HandlerContext) =>
    this.#execStream("AttachExec", req, ctx);

  async *#execStream(
    rpc: "Exec" | "AttachExec",
    req: ExecRequest | AttachExecRequest,
    ctx: HandlerContext,
  ): AsyncGenerator<ExecEvent> {
    this.#checkAuth(ctx, rpc);
    const n = this.#streamCount++;
    const start = this.clock.now();
    const attach =
      rpc === "AttachExec" ? (req as AttachExecRequest) : undefined;
    this.calls.push({
      rpc,
      at: start,
      grantId: ctx.requestHeader.get("ownsand-relay-grant-id") ?? undefined,
      operationId: req.execId,
      stdoutOffset: attach?.stdoutOffset,
      stderrOffset: attach?.stderrOffset,
      salt: attach ? undefined : (req as ExecRequest).bindingSalt,
    });
    const plan = this.hooks.stream?.(rpc, n);
    this.#applyPlanHeaders(plan, ctx);
    if (plan?.reject) {
      throw plan.reject;
    }
    const g = this.#admit(
      ctx,
      rpc === "Exec"
        ? [RelayOperationKind.EXEC]
        : [RelayOperationKind.EXEC, RelayOperationKind.ATTACH_EXEC],
      req.execId,
    );
    const close = this.#track(g, ctx);
    try {
      const ka = () =>
        create(ExecEventSchema, { event: { case: "keepalive", value: {} } });
      const accept = yield* this.#beforeAccept(plan, ctx, ka);
      if (!accept) {
        return;
      }
      // Agent-side checks of the first request frame.
      let e = this.execs.get(req.execId);
      if (rpc === "Exec") {
        const r = req as ExecRequest;
        const digest = bindingDigest(execBinding(r));
        if (!sameBytes(digest, g.digest)) {
          throw new ConnectError("digest mismatch", Code.PermissionDenied);
        }
        if (e) {
          if (!sameBytes(e.digest, digest)) {
            throw new ConnectError("exec_id reused", Code.AlreadyExists);
          }
        } else {
          if (this.sandboxState !== SandboxState.RUNNING) {
            throw new ConnectError(
              "sandbox is not running",
              Code.FailedPrecondition,
            );
          }
          e = this.#startExec(r, digest);
        }
      } else {
        if (!e) {
          throw new ConnectError("exec not found", Code.NotFound);
        }
        if (
          g.kind === RelayOperationKind.EXEC &&
          !sameBytes(g.digest, e.digest)
        ) {
          throw new ConnectError("digest mismatch", Code.PermissionDenied);
        }
      }
      yield create(ExecEventSchema, { event: { case: "accepted", value: {} } });
      const acceptedAt = this.clock.now();
      yield create(ExecEventSchema, {
        event: { case: "started", value: { execId: e.execId } },
      });
      let so = attach?.stdoutOffset ?? 0n;
      let se = attach?.stderrOffset ?? 0n;
      if (so < e.keptStdout || se < e.keptStderr) {
        so = so < e.keptStdout ? e.keptStdout : so;
        se = se < e.keptStderr ? e.keptStderr : se;
        yield create(ExecEventSchema, {
          event: {
            case: "outputTruncated",
            value: { firstStdoutOffset: so, firstStderrOffset: se },
          },
        });
      }
      if (plan?.stallAfterAccept) {
        await this.clock.sleep(24 * 60 * MIN, ctx.signal);
        return;
      }
      let sent = 0;
      let lastSend = this.clock.now();
      for (;;) {
        for (const o of e.outputs) {
          const isErr = o.stream === OutputStream.STDERR;
          const cur = isErr ? se : so;
          const end = o.offset + BigInt(o.data.byteLength);
          if (end <= cur || o.offset < (isErr ? e.keptStderr : e.keptStdout)) {
            continue;
          }
          yield create(ExecEventSchema, {
            event: {
              case: "output",
              value: { stream: o.stream, offset: o.offset, data: o.data },
            },
          });
          sent++;
          if (
            plan?.disconnectAfterData !== undefined &&
            sent >= plan.disconnectAfterData
          ) {
            throw disconnect();
          }
          lastSend = this.clock.now();
          if (isErr) {
            se = end;
          } else {
            so = end;
          }
        }
        if (e.final) {
          yield create(ExecEventSchema, { event: e.final });
          return;
        }
        if (this.sandboxState === SandboxState.PAUSED) {
          yield create(ExecEventSchema, {
            event: { case: "paused", value: {} },
          });
          return;
        }
        if (
          this.sandboxState === SandboxState.DELETED ||
          this.sandboxState === SandboxState.FAILED
        ) {
          yield create(ExecEventSchema, {
            event: {
              case: "sandboxEnded",
              value: { reason: this.sandboxEndReason },
            },
          });
          return;
        }
        const now = this.clock.now();
        if (now - start >= ROTATION_MS) {
          return; // Rotation: OK status, no final event.
        }
        if (
          plan?.disconnectAfterMs !== undefined &&
          now - acceptedAt >= plan.disconnectAfterMs
        ) {
          throw disconnect();
        }
        if (now - lastSend >= KEEPALIVE_MS) {
          yield create(ExecEventSchema, {
            event: { case: "keepalive", value: {} },
          });
          lastSend = now;
        }
        const deadlines = [start + ROTATION_MS, lastSend + KEEPALIVE_MS];
        if (plan?.disconnectAfterMs !== undefined) {
          deadlines.push(acceptedAt + plan.disconnectAfterMs);
        }
        await this.#waitChange(
          e,
          Math.max(0, Math.min(...deadlines) - now),
          ctx.signal,
        );
      }
    } finally {
      close();
    }
  }

  async #waitChange(
    e: FakeExec,
    ms: number,
    signal: AbortSignal,
  ): Promise<void> {
    const stop = new AbortController();
    const onAbort = () => stop.abort();
    signal.addEventListener("abort", onAbort, { once: true });
    let wake!: () => void;
    const changed = new Promise<void>((resolve) => (wake = resolve));
    e.waiters.add(wake);
    try {
      await Promise.race([
        this.clock.sleep(ms, stop.signal).catch(() => {}),
        changed,
      ]);
    } finally {
      stop.abort();
      e.waiters.delete(wake);
      signal.removeEventListener("abort", onAbort);
    }
    signal.throwIfAborted();
  }

  // ------------------------------------------------------------------ read

  readFile = async function* (
    this: FakeOwnSand,
    req: ReadFileRequest,
    ctx: HandlerContext,
  ): AsyncGenerator<ReadFileResponse> {
    this.#checkAuth(ctx, "ReadFile");
    const n = this.#streamCount++;
    const start = this.clock.now();
    this.calls.push({
      rpc: "ReadFile",
      at: start,
      grantId: ctx.requestHeader.get("ownsand-relay-grant-id") ?? undefined,
      operationId: req.readId,
      offset: req.offset,
      salt: req.bindingSalt,
      path: req.path,
    });
    const plan = this.hooks.stream?.("ReadFile", n);
    this.#applyPlanHeaders(plan, ctx);
    if (plan?.reject) {
      throw plan.reject;
    }
    const g = this.#admit(ctx, [RelayOperationKind.READ_FILE], req.readId);
    const close = this.#track(g, ctx);
    try {
      const ka = () =>
        create(ReadFileResponseSchema, {
          message: { case: "keepalive", value: {} },
        });
      const accept = yield* this.#beforeAccept(plan, ctx, ka);
      if (!accept) {
        return;
      }
      const digest = bindingDigest(
        readBinding(req.bindingSalt, req.sandboxId, req.readId, req.path),
      );
      if (!sameBytes(digest, g.digest)) {
        throw new ConnectError("digest mismatch", Code.PermissionDenied);
      }
      let read = this.reads.get(req.readId);
      if (!read) {
        const data = this.files.get(req.path);
        if (!data) {
          throw new ConnectError("no such file", Code.NotFound);
        }
        read = { data: data.slice(), digest };
        this.reads.set(req.readId, read);
      } else if (!sameBytes(read.digest, digest)) {
        throw new ConnectError("digest mismatch", Code.PermissionDenied);
      }
      yield create(ReadFileResponseSchema, {
        message: { case: "accepted", value: {} },
      });
      if (plan?.stallAfterAccept) {
        await this.clock.sleep(24 * 60 * MIN, ctx.signal);
        return;
      }
      let offset = Number(req.offset);
      let sent = 0;
      while (offset < read.data.byteLength) {
        if (plan?.disconnectAfterData === 0) {
          throw disconnect();
        }
        const end = Math.min(
          offset + this.readChunkBytes,
          read.data.byteLength,
        );
        yield create(ReadFileResponseSchema, {
          message: {
            case: "chunk",
            value: {
              offset: BigInt(offset),
              data: read.data.slice(offset, end),
            },
          },
        });
        sent++;
        if (
          plan?.disconnectAfterData !== undefined &&
          sent >= plan.disconnectAfterData
        ) {
          throw disconnect();
        }
        offset = end;
        if (this.clock.now() - start >= ROTATION_MS) {
          return;
        }
      }
      yield create(ReadFileResponseSchema, {
        message: {
          case: "finished",
          value: { sizeBytes: BigInt(read.data.byteLength) },
        },
      });
    } finally {
      close();
    }
  };

  // ----------------------------------------------------------------- write

  writeFile = async (req: WriteFileRequest, ctx: HandlerContext) => {
    this.#checkAuth(ctx, "WriteFile");
    const n = this.#writeCount++;
    this.calls.push({
      rpc: "WriteFile",
      at: this.clock.now(),
      grantId: ctx.requestHeader.get("ownsand-relay-grant-id") ?? undefined,
      operationId: req.writeId,
      writeOffset: req.chunk?.offset,
      finish: req.finish !== undefined,
      salt: req.bindingSalt,
      path: req.path,
    });
    const plan = this.hooks.write?.(req, n);
    if (plan?.reject) {
      throw plan.reject;
    }
    const g = this.#admit(ctx, [RelayOperationKind.WRITE_FILE], req.writeId);
    const close = this.#track(g, ctx);
    try {
      if (plan?.hang) {
        await this.clock.sleep(24 * 60 * MIN, ctx.signal);
      }
      if (plan?.deadlineAfterMs !== undefined) {
        await this.clock.sleep(plan.deadlineAfterMs, ctx.signal);
        throw new ConnectError(
          "relay WriteFile call limit",
          Code.DeadlineExceeded,
        );
      }
      let w = this.writes.get(req.writeId);
      if (!w) {
        if (!req.path) {
          throw new ConnectError("unknown write", Code.NotFound);
        }
        const digest = bindingDigest(
          writeBinding(req.bindingSalt, req.sandboxId, req.writeId, req.path),
        );
        if (!sameBytes(digest, g.digest)) {
          throw new ConnectError("digest mismatch", Code.PermissionDenied);
        }
        w = {
          digest,
          path: req.path,
          buf: new Uint8Array(0),
          written: [],
          finished: false,
        };
        this.writes.set(req.writeId, w);
      } else if (!sameBytes(w.digest, g.digest)) {
        throw new ConnectError("digest mismatch", Code.PermissionDenied);
      }
      if (req.chunk) {
        const off = Number(req.chunk.offset);
        const data = req.chunk.data;
        if (w.buf.byteLength < off + data.byteLength) {
          const grown = new Uint8Array(off + data.byteLength);
          grown.set(w.buf);
          w.buf = grown;
        }
        for (let i = 0; i < data.byteLength; i++) {
          if (w.written[off + i] && w.buf[off + i] !== data[i]) {
            throw new ConnectError(
              "overlapping chunk differs",
              Code.FailedPrecondition,
            );
          }
          w.buf[off + i] = data[i]!;
          w.written[off + i] = true;
        }
      }
      if (req.finish) {
        const size = Number(req.finish.sizeBytes);
        for (let i = 0; i < size; i++) {
          if (!w.written[i]) {
            throw new ConnectError(
              "written ranges do not cover the file",
              Code.FailedPrecondition,
            );
          }
        }
        if (w.buf.byteLength !== size) {
          throw new ConnectError(
            "written ranges exceed the size",
            Code.FailedPrecondition,
          );
        }
        w.finished = true;
        w.size = req.finish.sizeBytes;
        this.files.set(w.path, w.buf.slice());
      }
      if (plan?.disconnectAfterApply) {
        throw disconnect();
      }
      let contiguous = 0;
      while (w.written[contiguous]) {
        contiguous++;
      }
      return { writtenBytes: BigInt(contiguous), finished: w.finished };
    } finally {
      close();
    }
  };

  // --------------------------------------------------------------- router

  routes = (router: ConnectRouter): void => {
    const sandbox = () =>
      create(SandboxSchema, {
        organizationId: ORG,
        sandboxId: SANDBOX,
        state: this.sandboxState,
        endReason: this.sandboxEndReason,
      });
    router.service(DataPathService, {
      authorizeRelay: this.authorizeRelay,
      openSession: (_req, ctx) => {
        this.#checkAuth(ctx, "OpenSession");
        this.openSessionCalls++;
        throw new ConnectError("not used", Code.Unimplemented);
      },
      getIceServers: (_req, ctx) => {
        this.#checkAuth(ctx, "GetIceServers");
        return { iceServers: [{ urls: ["stun:ownsand.api.delino.io:3478"] }] };
      },
    });
    router.service(RelayService, {
      exec: this.exec,
      attachExec: this.attachExec,
      readFile: (req, ctx) => this.readFile(req, ctx),
      writeFile: this.writeFile,
    });
    router.service(SandboxService, {
      getSandbox: (req, ctx) => {
        this.#checkAuth(ctx, "GetSandbox");
        this.calls.push({ rpc: "GetSandbox", at: this.clock.now() });
        if (req.organizationId !== ORG) {
          throw new ConnectError("wrong organization", Code.NotFound);
        }
        return { sandbox: sandbox() };
      },
      createSandbox: (req, ctx) => {
        this.#checkAuth(ctx, "CreateSandbox");
        this.calls.push({
          rpc: "CreateSandbox",
          at: this.clock.now(),
          authorizationId: req.idempotencyKey,
        });
        const n = this.#createCount++;
        const id =
          this.createKeys.get(req.idempotencyKey) ?? this.#id("sandbox");
        this.createKeys.set(req.idempotencyKey, id);
        if (this.hooks.createSandbox?.(n) === "lose") {
          throw disconnect();
        }
        return {
          sandbox: create(SandboxSchema, {
            organizationId: req.organizationId,
            sandboxId: id,
            state: SandboxState.CREATING,
          }),
        };
      },
      pauseSandbox: (_req, ctx) => {
        this.#checkAuth(ctx, "PauseSandbox");
        this.setSandboxState(SandboxState.PAUSED);
        return { sandbox: sandbox() };
      },
      resumeSandbox: (_req, ctx) => {
        this.#checkAuth(ctx, "ResumeSandbox");
        this.setSandboxState(SandboxState.RUNNING);
        return { sandbox: sandbox() };
      },
      deleteSandbox: (_req, ctx) => {
        this.#checkAuth(ctx, "DeleteSandbox");
        this.setSandboxState(
          SandboxState.DELETED,
          SandboxEndReason.DELETED_BY_CALLER,
        );
        return { sandbox: sandbox() };
      },
      listSandboxes: (req, ctx) => {
        this.#checkAuth(ctx, "ListSandboxes");
        this.calls.push({
          rpc: "ListSandboxes",
          at: this.clock.now(),
          operationId: req.organizationId,
        });
        return { sandboxes: [sandbox()] };
      },
    });
    router.service(SnapshotService, {
      createSnapshot: (req, ctx) => {
        this.#checkAuth(ctx, "CreateSnapshot");
        return {
          snapshot: {
            organizationId: req.organizationId,
            snapshotId: this.#id("snapshot"),
            sandboxId: req.sandboxId,
          },
        };
      },
    });
    router.service(OrganizationService, {
      getOrganization: (req, ctx) => {
        this.#checkAuth(ctx, "GetOrganization");
        this.calls.push({
          rpc: "GetOrganization",
          at: this.clock.now(),
          operationId: req.organizationId,
        });
        return { organization: { organizationId: req.organizationId } };
      },
      listOrganizations: (_req, ctx) => {
        this.#checkAuth(ctx, "ListOrganizations");
        this.calls.push({ rpc: "ListOrganizations", at: this.clock.now() });
        return { organizations: [] };
      },
    });
  };
}

export function sameBytes(a: Uint8Array, b: Uint8Array): boolean {
  return a.byteLength === b.byteLength && a.every((x, i) => x === b[i]);
}
