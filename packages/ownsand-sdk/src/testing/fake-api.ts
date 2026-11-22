// SPDX-License-Identifier: Apache-2.0
import {
  create,
  type DescMessage,
  type DescMethod,
  fromBinary,
  type Message,
  toBinary,
} from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { ErrorDetailSchema } from "@delino/ownsand-protos/v1/common_pb";
import { ApiKeyService } from "@delino/ownsand-protos/v1/api_key_pb";
import { BillingService } from "@delino/ownsand-protos/v1/billing_pb";
import { DataPathService } from "@delino/ownsand-protos/v1/data_path_pb";
import { DeviceService } from "@delino/ownsand-protos/v1/device_pb";
import { MemberService } from "@delino/ownsand-protos/v1/member_pb";
import { OrganizationService } from "@delino/ownsand-protos/v1/organization_pb";
import { RelayService } from "@delino/ownsand-protos/v1/relay_pb";
import { SandboxService } from "@delino/ownsand-protos/v1/sandbox_pb";
import { SnapshotService } from "@delino/ownsand-protos/v1/snapshot_pb";
import type { VirtualClock } from "./virtual-clock.ts";

/** Waits in virtual time inside a stream script. */
export class Sleep {
  constructor(readonly ms: number) {}
}
/** Ends the response body with a transport error (no end-of-stream message). */
export class Disconnect {}
/** Ends the response body cleanly but without the end-of-stream message. */
export class Eof {}
/** Sends one message in byte pieces with a delay between pieces. */
export class Trickle {
  constructor(
    readonly message: Message,
    readonly pieces: number,
    readonly gapMs: number,
  ) {}
}
/** Hangs until the client cancels the call. */
export class Hang {}

export type StreamStep = Message | Sleep | Disconnect | Eof | Trickle | Hang;

export interface CallRecord {
  path: string;
  method: string;
  headers: Headers;
  request: Message;
}

export interface CallContext {
  headers: Headers;
  signal: AbortSignal;
  /** The number of earlier calls of this method. */
  index: number;
}

export type UnaryHandler = (
  request: any,
  call: CallContext,
) =>
  | Promise<Message | Record<string, unknown>>
  | Message
  | Record<string, unknown>;
export type StreamHandler = (
  request: any,
  call: CallContext,
) => AsyncIterable<StreamStep> | Iterable<StreamStep>;

const SERVICES = [
  OrganizationService,
  MemberService,
  DeviceService,
  ApiKeyService,
  SandboxService,
  SnapshotService,
  DataPathService,
  RelayService,
  BillingService,
];

const METHODS = new Map<string, DescMethod>();
for (const service of SERVICES) {
  for (const method of service.methods) {
    METHODS.set(`/${service.typeName}/${method.name}`, method);
  }
}

/** Thrown by a unary handler to make fetch reject (a lost response). */
export class LostResponse extends Error {}

const HTTP_STATUS: Partial<Record<Code, number>> = {
  [Code.InvalidArgument]: 400,
  [Code.Unauthenticated]: 401,
  [Code.PermissionDenied]: 403,
  [Code.NotFound]: 404,
  [Code.AlreadyExists]: 409,
  [Code.FailedPrecondition]: 412,
  [Code.ResourceExhausted]: 429,
  [Code.Unavailable]: 503,
  [Code.DeadlineExceeded]: 504,
};

const CODE_NAMES: Record<number, string> = {
  [Code.Canceled]: "canceled",
  [Code.Unknown]: "unknown",
  [Code.InvalidArgument]: "invalid_argument",
  [Code.DeadlineExceeded]: "deadline_exceeded",
  [Code.NotFound]: "not_found",
  [Code.AlreadyExists]: "already_exists",
  [Code.PermissionDenied]: "permission_denied",
  [Code.ResourceExhausted]: "resource_exhausted",
  [Code.FailedPrecondition]: "failed_precondition",
  [Code.Aborted]: "aborted",
  [Code.OutOfRange]: "out_of_range",
  [Code.Unimplemented]: "unimplemented",
  [Code.Internal]: "internal",
  [Code.Unavailable]: "unavailable",
  [Code.DataLoss]: "data_loss",
  [Code.Unauthenticated]: "unauthenticated",
};

function errorJson(error: ConnectError): Record<string, unknown> {
  const details = error.findDetails(ErrorDetailSchema).map((detail) => ({
    type: ErrorDetailSchema.typeName,
    value: Buffer.from(toBinary(ErrorDetailSchema, detail))
      .toString("base64")
      .replace(/=+$/, ""),
  }));
  return {
    code: CODE_NAMES[error.code] ?? "unknown",
    message: error.rawMessage,
    ...(details.length > 0 ? { details } : {}),
  };
}

function envelope(flags: number, data: Uint8Array): Uint8Array {
  const out = new Uint8Array(5 + data.length);
  out[0] = flags;
  new DataView(out.buffer).setUint32(1, data.length);
  out.set(data, 5);
  return out;
}

/**
 * An in-memory control plane that speaks the Connect protocol (binary) over
 * a `fetch` function, so the SDK runs its real Connect client stack. Tests
 * register handlers per method and read the call log.
 */
export class FakeApi {
  readonly origin: string;
  readonly calls: CallRecord[] = [];
  readonly #clock: VirtualClock;
  readonly #unary = new Map<string, UnaryHandler>();
  readonly #stream = new Map<string, StreamHandler>();
  readonly #counts = new Map<string, number>();
  /** URLs of every fetch, including refused ones. */
  readonly fetchedUrls: string[] = [];

  constructor(clock: VirtualClock, origin = "https://ownsand.api.test") {
    this.#clock = clock;
    this.origin = origin;
  }

  unary(
    service: { typeName: string },
    method: string,
    handler: UnaryHandler,
  ): this {
    this.#unary.set(`/${service.typeName}/${method}`, handler);
    return this;
  }

  stream(
    service: { typeName: string },
    method: string,
    handler: StreamHandler,
  ): this {
    this.#stream.set(`/${service.typeName}/${method}`, handler);
    return this;
  }

  /** Calls of one method, in order. */
  callsOf(method: string): CallRecord[] {
    return this.calls.filter((c) => c.method === method);
  }

  readonly fetch: typeof globalThis.fetch = async (input, init) => {
    const url = new URL(
      typeof input === "string"
        ? input
        : input instanceof URL
          ? input.href
          : input.url,
    );
    this.fetchedUrls.push(url.href);
    const path = url.pathname;
    const method = METHODS.get(path);
    if (method === undefined) {
      return new Response(null, { status: 404 });
    }
    const headers = new Headers(init?.headers);
    const body = new Uint8Array(
      await new Response(init?.body as BodyInit).arrayBuffer(),
    );
    const signal = init?.signal ?? new AbortController().signal;
    const index = this.#counts.get(path) ?? 0;
    this.#counts.set(path, index + 1);
    const call: CallContext = { headers, signal, index };
    if (method.methodKind === "unary") {
      const request = fromBinary(method.input, body);
      this.calls.push({ path, method: method.name, headers, request });
      const handler = this.#unary.get(path);
      if (handler === undefined) {
        return this.#unaryError(
          new ConnectError(`no fake for ${path}`, Code.Unimplemented),
        );
      }
      // Like a real fetch, a unary call whose signal aborts rejects with the
      // signal's reason even while the handler has not answered, so a stalled
      // handler (one that never settles) does not outlive its caller.
      let onAbort: (() => void) | undefined;
      const aborted = new Promise<never>((_, reject) => {
        if (signal.aborted) {
          reject(signal.reason);
          return;
        }
        onAbort = () => reject(signal.reason);
        signal.addEventListener("abort", onAbort, { once: true });
      });
      aborted.catch(() => {});
      try {
        const response = await Promise.race([
          (async () => handler(request, call))(),
          aborted,
        ]);
        return new Response(
          toBinary(method.output, toMessage(method.output, response)),
          { status: 200, headers: { "content-type": "application/proto" } },
        );
      } catch (error) {
        if (signal.aborted && error === signal.reason) {
          throw error;
        }
        if (error instanceof LostResponse) {
          throw new TypeError("fetch failed: connection reset");
        }
        return this.#unaryError(ConnectError.from(error));
      } finally {
        if (onAbort !== undefined) {
          signal.removeEventListener("abort", onAbort);
        }
      }
    }
    // Server streaming: one enveloped request message.
    const request = fromBinary(method.input, body.subarray(5));
    this.calls.push({ path, method: method.name, headers, request });
    const handler = this.#stream.get(path);
    const clock = this.#clock;
    const output = method.output;
    const steps = handler
      ? (async function* () {
          yield* handler(request, call);
        })()
      : (async function* (): AsyncGenerator<StreamStep> {
          throw new ConnectError(`no fake for ${path}`, Code.Unimplemented);
        })();
    let finished = false;
    const stream = new ReadableStream<Uint8Array>({
      async pull(controller) {
        if (finished) return;
        let next: IteratorResult<StreamStep>;
        try {
          next = await steps.next();
        } catch (error) {
          finished = true;
          controller.enqueue(
            envelope(
              2,
              new TextEncoder().encode(
                JSON.stringify({ error: errorJson(ConnectError.from(error)) }),
              ),
            ),
          );
          controller.close();
          return;
        }
        if (next.done) {
          finished = true;
          controller.enqueue(envelope(2, new TextEncoder().encode("{}")));
          controller.close();
          return;
        }
        const step = next.value;
        if (step instanceof Sleep) {
          await clock.sleep(step.ms, signal).catch(() => {});
          return;
        }
        if (step instanceof Disconnect) {
          finished = true;
          controller.error(new TypeError("terminated: other side closed"));
          return;
        }
        if (step instanceof Eof) {
          finished = true;
          controller.close();
          return;
        }
        if (step instanceof Hang) {
          await new Promise<void>((resolve) => {
            if (signal.aborted) resolve();
            signal.addEventListener("abort", () => resolve(), { once: true });
          });
          finished = true;
          controller.error(signal.reason ?? new TypeError("aborted"));
          return;
        }
        if (step instanceof Trickle) {
          const bytes = envelope(0, toBinary(output, step.message as never));
          const size = Math.ceil(bytes.length / step.pieces);
          for (let i = 0; i < bytes.length; i += size) {
            if (i > 0) {
              await clock.sleep(step.gapMs, signal).catch(() => {});
            }
            controller.enqueue(bytes.subarray(i, i + size));
          }
          return;
        }
        controller.enqueue(envelope(0, toBinary(output, step as never)));
      },
      async cancel() {
        finished = true;
        await steps.return(undefined);
      },
    });
    return new Response(stream, {
      status: 200,
      headers: { "content-type": "application/connect+proto" },
    });
  };

  #unaryError(error: ConnectError): Response {
    return new Response(JSON.stringify(errorJson(error)), {
      status: HTTP_STATUS[error.code] ?? 500,
      headers: { "content-type": "application/json" },
    });
  }
}

function toMessage(
  schema: DescMessage,
  value: Message | Record<string, unknown>,
): Message {
  return "$typeName" in value
    ? (value as Message)
    : create(schema, value as never);
}

/** Builds a ConnectError with an OwnSand ErrorDetail. */
export function ownsandError(
  code: Code,
  message: string,
  detail: {
    retryAfterMs?: number;
    sessionEndReason?: number;
    billingReason?: number;
  } = {},
): ConnectError {
  return new ConnectError(message, code, undefined, [
    {
      desc: ErrorDetailSchema,
      value: {
        code: code as number,
        message,
        retryAfter:
          detail.retryAfterMs === undefined
            ? undefined
            : {
                seconds: BigInt(Math.floor(detail.retryAfterMs / 1000)),
                nanos: (detail.retryAfterMs % 1000) * 1e6,
              },
        sessionEndReason: detail.sessionEndReason ?? 0,
        billingReason: detail.billingReason ?? 0,
      },
    },
  ]);
}
