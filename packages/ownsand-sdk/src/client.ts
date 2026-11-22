// SPDX-License-Identifier: Apache-2.0
import { create } from "@bufbuild/protobuf";
import {
  type Client,
  Code,
  ConnectError,
  createClient,
  type Transport,
} from "@connectrpc/connect";
import { type Clock, systemClock } from "./clock.ts";
import {
  ActivityRegistry,
  createApiTransport,
  type Credential,
  type Env,
  EnvVar,
  processEnv,
  resolveApiOrigin,
  resolveCredential,
} from "./connection.ts";
import {
  classifyFailure,
  FailureClass,
  OwnSandConfigError,
  OwnSandCredentialError,
  OwnSandError,
  retryAfterMs,
} from "./errors.ts";
import { ApiKeyService } from "@delino/ownsand-protos/v1/api_key_pb";
import { BillingService } from "@delino/ownsand-protos/v1/billing_pb";
import type {
  Arch,
  BaseImage,
  DataPathTransport,
  HostOs,
  NetworkMode,
} from "@delino/ownsand-protos/v1/common_pb";
import { DataPathService } from "@delino/ownsand-protos/v1/data_path_pb";
import { DeviceService } from "@delino/ownsand-protos/v1/device_pb";
import { MemberService } from "@delino/ownsand-protos/v1/member_pb";
import { OrganizationService } from "@delino/ownsand-protos/v1/organization_pb";
import { RelayService } from "@delino/ownsand-protos/v1/relay_pb";
import {
  CreateSandboxRequestSchema,
  SandboxService,
} from "@delino/ownsand-protos/v1/sandbox_pb";
import {
  type Snapshot,
  SnapshotService,
  type Template,
} from "@delino/ownsand-protos/v1/snapshot_pb";
import { assertTransportAvailable } from "./datapath.ts";
import type { Logger } from "./logger.ts";
import {
  type PeerConnectionConstructor,
  runtimePeerConnection,
} from "./p2p/webrtc.ts";
import { type OperationContext } from "./relay/context.ts";
import { backoffMs, FixedBudget } from "./relay/recovery.ts";
import { Sandbox } from "./sandbox.ts";
import { canonicalizeUuidV7, UuidV7Generator } from "./uuidv7.ts";
import {
  durationFromMs,
  validateLabels,
  validateResources,
  validateTtl,
} from "./validate.ts";

export interface OwnSandClientOptions {
  /** API origin. Default: https://ownsand.api.delino.io. */
  origin?: string;
  /** An organization API key. */
  apiKey?: string;
  /** A function that returns a current Logto access token. */
  accessToken?: () => string | Promise<string>;
  /** Default organization; `OWNSAND_ORG` when absent. */
  organizationId?: string;
  /** Forces a data path transport, for example for tests. */
  transport?: DataPathTransport;
  /**
   * WebRTC implementation for the peer-to-peer data path. Default: the
   * runtime's `RTCPeerConnection`, if any. `false` disables peer-to-peer.
   */
  webrtc?: { RTCPeerConnection: PeerConnectionConstructor } | false;
  /** Optional logger. The SDK logs nothing without one. */
  logger?: Logger;
  /** Fetch implementation. Default: the global fetch. */
  fetch?: typeof globalThis.fetch;
  /** Time source. Default: the system clock. */
  clock?: Clock;
  /** Environment. Default: the process environment. */
  env?: Env;
}

/** Exactly one sandbox source. */
export type SandboxSourceInput =
  { baseImage: BaseImage } | { snapshotId: string } | { templateId: string };

export interface CreateSandboxInput {
  source: SandboxSourceInput;
  /**
   * Resources of the sandbox. `memoryMib` and `diskGib` are `int64` fields on
   * the wire, so each accepts a `number` (a safe integer) or a `bigint` (up to
   * the `int64` maximum); `vcpuCount` is an `int32` and stays a `number`.
   */
  resources: {
    vcpuCount: number;
    memoryMib: number | bigint;
    diskGib: number | bigint;
  };
  /** Idle TTL in milliseconds. Required; there is no default. */
  idleTtl: number;
  /** Maximum TTL in milliseconds, at most 7 days. Required. */
  maxTtl: number;
  placement?: { deviceId?: string; hostOs?: HostOs; arch?: Arch };
  network?: NetworkMode;
  labels?: Record<string, string>;
  organizationId?: string;
  signal?: AbortSignal;
}

/**
 * The OwnSand client: the public services with idiomatic names and sandbox
 * handles with the data path.
 */
export class OwnSandClient {
  readonly origin: string;
  readonly organizationId: string | undefined;
  readonly organizations: Client<typeof OrganizationService>;
  readonly members: Client<typeof MemberService>;
  readonly devices: Client<typeof DeviceService>;
  readonly apiKeys: Client<typeof ApiKeyService>;
  readonly sandboxes: Client<typeof SandboxService>;
  readonly snapshots: Client<typeof SnapshotService>;
  readonly dataPath: Client<typeof DataPathService>;
  readonly relay: Client<typeof RelayService>;
  /**
   * Billing status, usage and the billing account link. Payments, top-ups
   * and billing account management stay on https://accounts.delino.io.
   */
  readonly billing: Client<typeof BillingService>;
  readonly #clock: Clock;
  readonly #ids: UuidV7Generator;
  readonly #activity = new ActivityRegistry();
  readonly #logger: Logger | undefined;
  readonly #Peer: PeerConnectionConstructor | undefined;
  readonly #forced: DataPathTransport | undefined;

  constructor(options: OwnSandClientOptions = {}) {
    const env = options.env ?? processEnv();
    if (options.apiKey !== undefined && options.accessToken !== undefined) {
      throw new OwnSandConfigError("pass either apiKey or accessToken");
    }
    const explicit: Credential | undefined =
      options.apiKey !== undefined
        ? { apiKey: options.apiKey }
        : options.accessToken !== undefined
          ? { accessToken: options.accessToken }
          : undefined;
    const credential = resolveCredential(explicit, env);
    this.origin = resolveApiOrigin(options.origin);
    const org = options.organizationId ?? env[EnvVar.Organization];
    this.organizationId = org === "" ? undefined : org;
    this.#clock = options.clock ?? systemClock;
    this.#ids = new UuidV7Generator(() => this.#clock.wallNow());
    this.#logger = options.logger;
    this.#forced = options.transport;
    this.#Peer =
      options.webrtc === false
        ? undefined
        : (options.webrtc?.RTCPeerConnection ?? runtimePeerConnection());
    const transport: Transport = createApiTransport({
      origin: this.origin,
      credential,
      organizationId: this.organizationId,
      fetch: options.fetch ?? globalThis.fetch.bind(globalThis),
      logger: options.logger,
      activity: this.#activity,
      now: () => this.#clock.now(),
    });
    this.organizations = createClient(OrganizationService, transport);
    this.members = createClient(MemberService, transport);
    this.devices = createClient(DeviceService, transport);
    this.apiKeys = createClient(ApiKeyService, transport);
    this.sandboxes = createClient(SandboxService, transport);
    this.snapshots = createClient(SnapshotService, transport);
    this.dataPath = createClient(DataPathService, transport);
    this.relay = createClient(RelayService, transport);
    this.billing = createClient(BillingService, transport);
  }

  #org(override: string | undefined): string {
    const org = override ?? this.organizationId;
    if (org === undefined || org === "") {
      throw new OwnSandConfigError(
        `no organization: set organizationId or ${EnvVar.Organization}`,
      );
    }
    return org;
  }

  /** Returns a handle of an existing sandbox. */
  sandbox(
    sandboxId: string,
    options: { organizationId?: string } = {},
  ): Sandbox {
    return this.#handle(
      sandboxId,
      this.#org(options.organizationId),
      undefined,
    );
  }

  #handle(
    sandboxId: string,
    organizationId: string,
    info: Sandbox["info"],
  ): Sandbox {
    assertTransportAvailable(this.#forced, this.#Peer);
    const ctx: OperationContext = {
      clock: this.#clock,
      ids: this.#ids,
      dataPath: this.dataPath,
      relay: this.relay,
      sandboxes: this.sandboxes,
      activity: this.#activity,
      logger: this.#logger,
      organizationId,
      // A UUID v7 in either hex case is stored in lowercase, the spelling of
      // the control plane and the agent; any other value is kept as given
      // and request validation reports it.
      sandboxId: canonicalizeUuidV7(sandboxId),
    };
    return new Sandbox(ctx, this.snapshots, this.#Peer, this.#forced, info);
  }

  /**
   * Creates a sandbox. It validates the TTLs and resources before the
   * request, generates one idempotency key for this create and reuses it for
   * every retry after a lost response. The sandbox starts in `CREATING`;
   * use `waitUntilStable` on the handle. A forced `PEER_TO_PEER` transport
   * without a WebRTC implementation fails with an `OwnSandConfigError`
   * before any request, because the handle could not be built after it.
   */
  async createSandbox(input: CreateSandboxInput): Promise<Sandbox> {
    assertTransportAvailable(this.#forced, this.#Peer);
    validateTtl(input.idleTtl, input.maxTtl);
    validateResources(input.resources);
    validateLabels(input.labels);
    const organizationId = this.#org(input.organizationId);
    const source = input.source;
    const request = create(CreateSandboxRequestSchema, {
      organizationId,
      idempotencyKey: this.#ids.next(),
      source: {
        source:
          "baseImage" in source
            ? { case: "baseImage", value: source.baseImage }
            : "snapshotId" in source
              ? { case: "snapshotId", value: source.snapshotId }
              : { case: "templateId", value: source.templateId },
      },
      resources: {
        vcpuCount: input.resources.vcpuCount,
        memoryMib: BigInt(input.resources.memoryMib),
        diskGib: BigInt(input.resources.diskGib),
      },
      idleTtl: durationFromMs(input.idleTtl),
      maxTtl: durationFromMs(input.maxTtl),
      placement: input.placement,
      network:
        input.network === undefined ? undefined : { mode: input.network },
      labels: { ...input.labels },
    });
    // The retry window of the contract: it starts with the first retried
    // failure, lasts 60 seconds (`FixedBudget`) and also bounds each retry in
    // flight. `lastFailure` is what the create returns when the
    // window ends.
    let budget: FixedBudget | undefined;
    let lastFailure: unknown;
    for (let attempt = 0; ; attempt++) {
      // A caller abort stays terminal and sends no further call.
      input.signal?.throwIfAborted();
      const bound = new AbortController();
      const stopBound = new AbortController();
      let signal = input.signal;
      if (budget !== undefined) {
        // Cancel the retry with a timer, not with a Connect request timeout.
        this.#clock
          .sleep(Math.max(0, budget.remainingMs()), stopBound.signal)
          .then(
            () => bound.abort(),
            () => {},
          );
        signal =
          input.signal === undefined
            ? bound.signal
            : AbortSignal.any([input.signal, bound.signal]);
      }
      try {
        const res = await this.sandboxes.createSandbox(request, { signal });
        stopBound.abort();
        if (res.sandbox === undefined) {
          throw new OwnSandError("CreateSandbox returned no sandbox");
        }
        return this.#handle(res.sandbox.sandboxId, organizationId, res.sandbox);
      } catch (error) {
        // The timer of this attempt is no longer needed, also not while the
        // back-off sleeps.
        stopBound.abort();
        if (input.signal?.aborted) {
          throw input.signal.reason;
        }
        if (bound.signal.aborted) {
          // The window ended while the retry was in flight.
          throw lastFailure;
        }
        const failure = createFailureOf(error);
        if (failure === undefined) {
          throw error;
        }
        budget ??= new FixedBudget(this.#clock);
        // A rate limit waits at least as long as the server asks.
        const delay =
          failure === FailureClass.ResourceExhausted
            ? Math.max(backoffMs(attempt), retryAfterMs(error))
            : backoffMs(attempt);
        if (delay >= budget.remainingMs()) {
          throw error;
        }
        lastFailure = error;
        await this.#clock.sleep(delay, input.signal);
      }
    }
  }

  async getSnapshot(
    snapshotId: string,
    organizationId?: string,
  ): Promise<Snapshot> {
    const res = await this.snapshots.getSnapshot({
      organizationId: this.#org(organizationId),
      snapshotId,
    });
    return required(res.snapshot, "snapshot");
  }

  async deleteSnapshot(
    snapshotId: string,
    organizationId?: string,
  ): Promise<void> {
    await this.snapshots.deleteSnapshot({
      organizationId: this.#org(organizationId),
      snapshotId,
    });
  }

  /** Starts an export to R2. It returns at once with `EXPORTING`. */
  async exportSnapshot(
    snapshotId: string,
    organizationId?: string,
  ): Promise<Snapshot> {
    const res = await this.snapshots.exportSnapshot({
      organizationId: this.#org(organizationId),
      snapshotId,
    });
    return required(res.snapshot, "snapshot");
  }

  async createTemplate(
    snapshotId: string,
    name: string,
    organizationId?: string,
  ): Promise<Template> {
    const res = await this.snapshots.createTemplate({
      organizationId: this.#org(organizationId),
      snapshotId,
      name,
    });
    return required(res.template, "template");
  }

  async deleteTemplate(
    templateId: string,
    organizationId?: string,
  ): Promise<void> {
    await this.snapshots.deleteTemplate({
      organizationId: this.#org(organizationId),
      templateId,
    });
  }
}

/**
 * Classifies a failed `CreateSandbox` call: the class of a failure that a
 * retry with the same idempotency key can resolve, or `undefined` for a
 * failure that ends the create. A bare `OwnSandError` (a local failure such
 * as an empty response) and a terminal class, including a billing error and a
 * local credential failure, end it. `deadline_exceeded` is a lost response of
 * a create, so it is retried here although the shared relay classification
 * treats it as terminal.
 */
function createFailureOf(error: unknown): FailureClass | undefined {
  if (error instanceof OwnSandError && !(error instanceof ConnectError)) {
    return undefined;
  }
  const failure = classifyFailure(error);
  if (failure !== FailureClass.Terminal) {
    return failure;
  }
  return error instanceof ConnectError &&
    error.code === Code.DeadlineExceeded &&
    !(error.cause instanceof OwnSandCredentialError)
    ? FailureClass.Interruption
    : undefined;
}

function required<T>(value: T | undefined, name: string): T {
  if (value === undefined) {
    throw new OwnSandError(`the response carried no ${name}`);
  }
  return value;
}
