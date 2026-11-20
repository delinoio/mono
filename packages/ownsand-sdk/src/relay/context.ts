// SPDX-License-Identifier: Apache-2.0
import type { MessageInitShape } from "@bufbuild/protobuf";

import type { Clock } from "../clock.ts";
import type { Sandbox } from "../gen/delino/ownsand/v1/sandbox_pb.ts";
import type {
  AttachExecRequest,
  AuthorizeRelayRequestSchema,
  AuthorizeRelayResponse,
  ExecEvent,
  ExecRequest,
  ReadFileRequest,
  ReadFileResponse,
  WriteFileRequest,
  WriteFileResponse,
} from "../gen/delino/ownsand/v1/datapath_pb.ts";
import type { Logger } from "../log.ts";

/** Per-call controls of one relay RPC attempt. */
export interface RelayCall {
  signal: AbortSignal;
  /** Called whenever response body bytes arrive. */
  onBytes?: () => void;
}

/**
 * The control plane calls that the relay data path makes. The client
 * implements it with Connect; the interface keeps the relay rules independent
 * of the transport.
 */
export interface RelayContext {
  readonly clock: Clock;
  readonly log: Logger;
  readonly organizationId: string;
  readonly sandboxId: string;
  authorizeRelay(
    request: MessageInitShape<typeof AuthorizeRelayRequestSchema>,
    signal: AbortSignal,
  ): Promise<AuthorizeRelayResponse>;
  getSandbox(signal: AbortSignal): Promise<Sandbox>;
  exec(
    grantId: string,
    request: ExecRequest,
    call: RelayCall,
  ): AsyncIterable<ExecEvent>;
  attachExec(
    grantId: string,
    request: AttachExecRequest,
    call: RelayCall,
  ): AsyncIterable<ExecEvent>;
  readFile(
    grantId: string,
    request: ReadFileRequest,
    call: RelayCall,
  ): AsyncIterable<ReadFileResponse>;
  writeFile(
    grantId: string,
    request: WriteFileRequest,
    call: RelayCall,
  ): Promise<WriteFileResponse>;
}
