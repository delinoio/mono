// SPDX-License-Identifier: Apache-2.0
import type { DescMessage } from "@bufbuild/protobuf";
import {
  type CallOptions,
  Code,
  ConnectError,
  createClient,
  type Interceptor,
  type StreamResponse,
  type Transport,
} from "@connectrpc/connect";
import {
  createNodeHttpClient,
  Http2SessionManager,
} from "@connectrpc/connect-node";
import type { UniversalClientFn } from "@connectrpc/connect/protocol";
import { createTransport } from "@connectrpc/connect/protocol-connect";

import { type Clock, systemClock } from "./clock.ts";
import { localError, Redactor } from "./errors.ts";
import { DataPathTransport } from "./gen/delino/ownsand/v1/common_pb.ts";
import {
  DataPathService,
  RelayService,
} from "./gen/delino/ownsand/v1/datapath_pb.ts";
import {
  MemberService,
  ApiKeyService,
  DeviceService,
  OrganizationService,
} from "./gen/delino/ownsand/v1/organization_pb.ts";
import {
  type Sandbox as SandboxMessage,
  SandboxService,
  SnapshotService,
} from "./gen/delino/ownsand/v1/sandbox_pb.ts";
import {
  ACTIVITY_HEADER,
  ActivityRegistry,
  createSdkHttpClient,
  GRANT_HEADER,
} from "./http.ts";
import { newUuidV7 } from "./ids.ts";
import { LogEvent, type Logger, noopLogger } from "./log.ts";
import type { RelayCall, RelayContext } from "./relay/context.ts";
import {
  backoffMs,
  isRetryableRelayFailure,
  RecoveryWindow,
} from "./relay/recovery.ts";
import {
  type CreateSandboxOptions,
  createRequestFields,
  SandboxHandle,
  type SandboxHandleBackend,
  validateCreate,
} from "./sandbox.ts";
import { type OrgScopedClient, orgScopedClient } from "./services.ts";

/** The default API origin. */
export const DEFAULT_ORIGIN = "https://ownsand.api.delino.io";

/** Environment variables that the client reads. */
export enum EnvVar {
  API_KEY = "OWNSAND_API_KEY",
  ORGANIZATION = "OWNSAND_ORG",
}

export interface OwnSandClientOptions {
  /** The API origin. Default `https://ownsand.api.delino.io`. */
  origin?: string;
  /** An organization API key. Exclusive with `accessToken`. */
  apiKey?: string;
  /** Returns a current Logto access token. Exclusive with `apiKey`. */
  accessToken?: () => string | Promise<string>;
  /** Default organization for every RPC that needs one. Default `OWNSAND_ORG`. */
  organizationId?: string;
  /**
   * Forces a data path transport. Only `RELAY` is supported: the TypeScript
   * SDK has no approved WebRTC library, so `PEER_TO_PEER` fails.
   */
  transport?: DataPathTransport;
  /** Receives log records. The SDK logs nothing by default. */
  logger?: Logger;
  /** Time source; tests inject a virtual clock. */
  clock?: Clock;
  /** Environment to read `OWNSAND_API_KEY` and `OWNSAND_ORG` from. Default `process.env`. */
  env?: Record<string, string | undefined>;
  /**
   * The HTTP client below the SDK's credential and origin checks. Default: an
   * HTTP/2 client for the origin. Tests pass an in-memory client.
   */
  httpClient?: UniversalClientFn;
}

const LOOPBACK_HOSTS = new Set(["localhost", "127.0.0.1", "[::1]"]);

/** Validates an API origin and returns its canonical form. */
export function parseOrigin(value: string): string {
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    throw localError("the API origin is not a valid URL", Code.InvalidArgument);
  }
  if (
    (url.pathname !== "/" && url.pathname !== "") ||
    url.search ||
    url.hash ||
    url.username ||
    url.password
  ) {
    throw localError(
      "the API origin must be an origin without path, query or credentials",
      Code.InvalidArgument,
    );
  }
  if (url.protocol === "http:" && !LOOPBACK_HOSTS.has(url.hostname)) {
    // Plain HTTP would expose the credential; it is allowed only on loopback for local tests.
    throw localError(
      "the API origin must use https unless it is a loopback address",
      Code.InvalidArgument,
    );
  }
  if (url.protocol !== "https:" && url.protocol !== "http:") {
    throw localError("the API origin must use https", Code.InvalidArgument);
  }
  return url.origin;
}

/**
 * The OwnSand client: organization-scoped wrappers of every public service
 * and sandbox handles with relay exec and file operations.
 */
export class OwnSandClient {
  readonly origin: string;
  readonly clock: Clock;
  readonly organizations: OrgScopedClient<typeof OrganizationService>;
  readonly members: OrgScopedClient<typeof MemberService>;
  readonly apiKeys: OrgScopedClient<typeof ApiKeyService>;
  readonly devices: OrgScopedClient<typeof DeviceService>;
  readonly sandboxService: OrgScopedClient<typeof SandboxService>;
  readonly snapshots: OrgScopedClient<typeof SnapshotService>;
  /**
   * `DataPathService`. `openSession` fails: the TypeScript SDK is relay-only.
   * Exec and file operations call `authorizeRelay` themselves.
   */
  readonly dataPath: OrgScopedClient<typeof DataPathService>;
  readonly #organizationId: string | undefined;
  readonly #log: Logger;
  readonly #transport: Transport;
  readonly #activity = new ActivityRegistry();
  readonly #relay: ReturnType<typeof createClient<typeof RelayService>>;
  readonly #redactor = new Redactor();
  #session: Http2SessionManager | undefined;

  constructor(options: OwnSandClientOptions = {}) {
    const env = options.env ?? process.env;
    if (options.transport === DataPathTransport.PEER_TO_PEER) {
      throw localError(
        "the TypeScript SDK supports only the RELAY transport: no WebRTC library is approved for Node.js",
        Code.Unimplemented,
      );
    }
    this.origin = parseOrigin(options.origin ?? DEFAULT_ORIGIN);
    this.clock = options.clock ?? systemClock;
    this.#log = options.logger ?? noopLogger;
    this.#organizationId =
      options.organizationId || env[EnvVar.ORGANIZATION] || undefined;

    const credential = this.#credentialSource(options, env);
    const inner =
      options.httpClient ??
      createNodeHttpClient({
        httpVersion: "2",
        // Called for every request: reuse one HTTP/2 session manager for the
        // origin, which reconnects after a connection loss.
        sessionProvider: () =>
          (this.#session ??= new Http2SessionManager(this.origin)),
      });
    const httpClient = createSdkHttpClient({
      origin: this.origin,
      credential,
      inner,
      activity: this.#activity,
    });
    this.#transport = createTransport({
      baseUrl: this.origin,
      httpClient,
      useBinaryFormat: true,
      interceptors: [this.#interceptor()],
      acceptCompression: [],
      sendCompression: null,
      compressMinBytes: Number.MAX_SAFE_INTEGER,
      // A file chunk is at most 1 MiB plus its envelope.
      readMaxBytes: 4 * 1024 * 1024,
      writeMaxBytes: 4 * 1024 * 1024,
    });

    const org = () => this.#organizationId;
    this.organizations = orgScopedClient(
      OrganizationService,
      this.#transport,
      org,
    );
    this.members = orgScopedClient(MemberService, this.#transport, org);
    this.apiKeys = orgScopedClient(ApiKeyService, this.#transport, org);
    this.devices = orgScopedClient(DeviceService, this.#transport, org);
    this.sandboxService = orgScopedClient(SandboxService, this.#transport, org);
    this.snapshots = orgScopedClient(SnapshotService, this.#transport, org);
    this.dataPath = orgScopedClient(DataPathService, this.#transport, org, {
      openSession:
        "OpenSession is not supported: the TypeScript SDK is relay-only because no WebRTC library is approved for Node.js",
    });
    this.#relay = createClient(RelayService, this.#transport);
  }

  /** Closes the HTTP/2 connections of the client. Open calls fail. */
  close(): void {
    this.#session?.abort();
    this.#session = undefined;
  }

  /** The data path transport of every sandbox handle: `RELAY`. */
  get transport(): DataPathTransport {
    return DataPathTransport.RELAY;
  }

  /** Returns a handle to an existing sandbox. */
  sandbox(
    sandboxId: string,
    options: { organizationId?: string } = {},
  ): SandboxHandle {
    const organizationId = this.#requireOrganization(options.organizationId);
    this.#log({
      event: LogEvent.TRANSPORT_SELECTED,
      sandboxId,
      transport: DataPathTransport.RELAY,
    });
    return new SandboxHandle(sandboxId, organizationId, this.#backend());
  }

  /**
   * Creates a sandbox and returns its handle and the sandbox in `CREATING`.
   * One `idempotency_key` covers every retry of this create.
   */
  async create(
    options: CreateSandboxOptions,
  ): Promise<{ handle: SandboxHandle; sandbox: SandboxMessage }> {
    validateCreate(options);
    const organizationId = this.#requireOrganization(options.organizationId);
    const fields = createRequestFields(
      options,
      newUuidV7(this.clock.wallNow()),
    );
    const signal = options.signal ?? new AbortController().signal;
    const window = new RecoveryWindow(this.clock);
    for (let attempt = 0; ; attempt++) {
      try {
        const res = await this.sandboxService.createSandbox(
          { ...fields, organizationId },
          { signal },
        );
        const sandbox = res.sandbox;
        if (!sandbox) {
          throw new ConnectError(
            "CreateSandbox returned no sandbox",
            Code.Internal,
          );
        }
        return {
          handle: this.sandbox(sandbox.sandboxId, { organizationId }),
          sandbox,
        };
      } catch (err) {
        const code = ConnectError.from(err).code;
        if (
          signal.aborted ||
          !(isRetryableRelayFailure(err) && code !== Code.ResourceExhausted)
        ) {
          throw err;
        }
        const delay = backoffMs(attempt);
        if (window.remaining() <= delay) {
          throw err;
        }
        await this.clock.sleep(delay, signal);
      }
    }
  }

  /** Reads one sandbox. */
  async get(
    sandboxId: string,
    options: { organizationId?: string; signal?: AbortSignal } = {},
  ): Promise<SandboxMessage> {
    return this.sandbox(sandboxId, options).get(options);
  }

  /** Lists sandboxes. */
  list(
    request: Parameters<
      OrgScopedClient<typeof SandboxService>["listSandboxes"]
    >[0] = {},
    options?: CallOptions,
  ): ReturnType<OrgScopedClient<typeof SandboxService>["listSandboxes"]> {
    return this.sandboxService.listSandboxes(request, options);
  }

  /** Deletes a sandbox. */
  async delete(
    sandboxId: string,
    options: { organizationId?: string; signal?: AbortSignal } = {},
  ): Promise<SandboxMessage> {
    return this.sandbox(sandboxId, options).delete(options);
  }

  /** Pauses a sandbox. */
  async pause(
    sandboxId: string,
    options: { organizationId?: string; signal?: AbortSignal } = {},
  ): Promise<SandboxMessage> {
    return this.sandbox(sandboxId, options).pause(options);
  }

  /** Resumes a sandbox. */
  async resume(
    sandboxId: string,
    options: { organizationId?: string; signal?: AbortSignal } = {},
  ): Promise<SandboxMessage> {
    return this.sandbox(sandboxId, options).resume(options);
  }

  #requireOrganization(override: string | undefined): string {
    const org = override || this.#organizationId;
    if (!org) {
      throw localError(
        "this operation needs an organization: pass organizationId or set it on the client or in OWNSAND_ORG",
        Code.InvalidArgument,
      );
    }
    return org;
  }

  #credentialSource(
    options: OwnSandClientOptions,
    env: Record<string, string | undefined>,
  ): () => Promise<string> {
    if (options.apiKey !== undefined && options.accessToken !== undefined) {
      throw localError(
        "pass either apiKey or accessToken, not both",
        Code.InvalidArgument,
      );
    }
    if (options.accessToken) {
      const fn = options.accessToken;
      return async () => {
        const token = await fn();
        if (!token) {
          throw localError(
            "the access token function returned no token",
            Code.Unauthenticated,
          );
        }
        this.#redactor.add(token);
        return token;
      };
    }
    const key = options.apiKey ?? env[EnvVar.API_KEY];
    if (!key) {
      throw localError(
        "no credential: pass apiKey or accessToken, or set OWNSAND_API_KEY",
        Code.Unauthenticated,
      );
    }
    this.#redactor.add(key);
    return () => Promise.resolve(key);
  }

  /** Logs each RPC and removes credentials from every error that reaches the caller. */
  #interceptor(): Interceptor {
    return (next) => async (req) => {
      const start = this.clock.now();
      const rpc = req.method.name;
      const sandboxId = sandboxIdOf(req.stream ? undefined : req.message);
      const done = (code: Code | undefined) =>
        this.#log({
          event: LogEvent.RPC,
          rpc,
          sandboxId,
          durationMs: this.clock.now() - start,
          code,
        });
      let res;
      try {
        res = await next(req);
      } catch (err) {
        const e = this.#redactor.error(err);
        done(e.code);
        throw e;
      }
      if (!res.stream) {
        done(undefined);
        return res;
      }
      const redactor = this.#redactor;
      const message = res.message;
      async function* redacted() {
        try {
          yield* message;
          done(undefined);
        } catch (err) {
          const e = redactor.error(err);
          done(e.code);
          throw e;
        }
      }
      return { ...res, message: redacted() } as StreamResponse<
        DescMessage,
        DescMessage
      >;
    };
  }

  #backend(): SandboxHandleBackend {
    const sandboxOf = (res: { sandbox?: SandboxMessage }) => {
      if (!res.sandbox) {
        throw new ConnectError("the response has no sandbox", Code.Internal);
      }
      return res.sandbox;
    };
    return {
      relay: (sandboxId, organizationId) =>
        this.#relayContext(sandboxId, organizationId),
      getSandbox: async (organizationId, sandboxId, signal) =>
        sandboxOf(
          await this.sandboxService.getSandbox(
            { organizationId, sandboxId },
            { signal },
          ),
        ),
      deleteSandbox: async (organizationId, sandboxId, signal) =>
        sandboxOf(
          await this.sandboxService.deleteSandbox(
            { organizationId, sandboxId },
            { signal },
          ),
        ),
      pauseSandbox: async (organizationId, sandboxId, signal) =>
        sandboxOf(
          await this.sandboxService.pauseSandbox(
            { organizationId, sandboxId },
            { signal },
          ),
        ),
      resumeSandbox: async (organizationId, sandboxId, signal) =>
        sandboxOf(
          await this.sandboxService.resumeSandbox(
            { organizationId, sandboxId },
            { signal },
          ),
        ),
      createSnapshot: async (organizationId, sandboxId, signal) => {
        const res = await this.snapshots.createSnapshot(
          { organizationId, sandboxId },
          { signal },
        );
        if (!res.snapshot) {
          throw new ConnectError("the response has no snapshot", Code.Internal);
        }
        return res.snapshot;
      },
    };
  }

  #relayContext(sandboxId: string, organizationId: string): RelayContext {
    const relay = this.#relay;
    const activity = this.#activity;
    const callOptions = (grantId: string, call: RelayCall) => {
      const reg = call.onBytes ? activity.register(call.onBytes) : undefined;
      const headers = new Headers({ [GRANT_HEADER]: grantId });
      if (reg) {
        headers.set(ACTIVITY_HEADER, reg.id);
      }
      return {
        options: { signal: call.signal, headers },
        dispose: () => reg?.dispose(),
      };
    };
    async function* streamed<T>(
      open: () => AsyncIterable<T>,
      dispose: () => void,
    ): AsyncIterable<T> {
      try {
        yield* open();
      } finally {
        dispose();
      }
    }
    return {
      clock: this.clock,
      log: this.#log,
      organizationId,
      sandboxId,
      authorizeRelay: (request, signal) =>
        this.dataPath.authorizeRelay(request, { signal }),
      getSandbox: async (signal) => {
        const res = await this.sandboxService.getSandbox(
          { organizationId, sandboxId },
          { signal },
        );
        if (!res.sandbox) {
          throw new ConnectError("the response has no sandbox", Code.Internal);
        }
        return res.sandbox;
      },
      exec: (grantId, request, call) => {
        const o = callOptions(grantId, call);
        return streamed(() => relay.exec(request, o.options), o.dispose);
      },
      attachExec: (grantId, request, call) => {
        const o = callOptions(grantId, call);
        return streamed(() => relay.attachExec(request, o.options), o.dispose);
      },
      readFile: (grantId, request, call) => {
        const o = callOptions(grantId, call);
        return streamed(() => relay.readFile(request, o.options), o.dispose);
      },
      writeFile: async (grantId, request, call) => {
        const o = callOptions(grantId, call);
        try {
          return await relay.writeFile(request, o.options);
        } finally {
          o.dispose();
        }
      },
    };
  }
}

function sandboxIdOf(message: unknown): string | undefined {
  if (
    typeof message === "object" &&
    message !== null &&
    "sandboxId" in message
  ) {
    const v = (message as { sandboxId: unknown }).sandboxId;
    return typeof v === "string" && v ? v : undefined;
  }
  return undefined;
}
