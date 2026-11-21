// SPDX-License-Identifier: Apache-2.0
import { createHash, randomBytes } from "node:crypto";
import type { Duration } from "@bufbuild/protobuf/wkt";
import { RelayOperationKind } from "./gen/delino/ownsand/v1/common_pb.ts";
import type {
  ExecRequest,
  ReadFileRequest,
  WriteFileRequest,
} from "./gen/delino/ownsand/v1/data_frame_pb.ts";

/** Size of a binding salt: 32 random bytes per operation. */
export const BINDING_SALT_SIZE = 32;

/** Size of a request digest (SHA-256). */
export const REQUEST_DIGEST_SIZE = 32;

/**
 * The fields of a `RelayRequestBinding`. The request digest is the SHA-256 of
 * its protobuf encoding with fields in field-number order. The relay grants
 * section of docs/protos-ownsand-api-contract.md owns the rule; the Go
 * implementation is packages/ownsand-sdk-go/relaybinding, and both must
 * reproduce relaybinding/testdata/request-digest-vectors.json.
 */
export interface RelayRequestBindingFields {
  bindingSalt: Uint8Array;
  sandboxId: string;
  operationKind: RelayOperationKind;
  operationId: string;
  command?: readonly string[];
  /** Environment entries in any order; the encoder sorts them by key bytes. */
  env?: Readonly<Record<string, string>>;
  workingDirectory?: string;
  /** Present timeouts are encoded also when they are zero. */
  timeout?: Pick<Duration, "seconds" | "nanos">;
  path?: string;
}

/** Returns BINDING_SALT_SIZE random bytes. */
export function newBindingSalt(): Uint8Array {
  return new Uint8Array(randomBytes(BINDING_SALT_SIZE));
}

/** Binding of an exec request. */
export function execBinding(req: ExecRequest): RelayRequestBindingFields {
  return {
    bindingSalt: req.bindingSalt,
    sandboxId: req.sandboxId,
    operationKind: RelayOperationKind.EXEC,
    operationId: req.execId,
    command: req.command,
    env: req.env,
    workingDirectory: req.workingDirectory,
    timeout: req.timeout,
  };
}

/** Binding of a file read request. Every ReadFile call carries the path. */
export function readFileBinding(
  req: ReadFileRequest,
): RelayRequestBindingFields {
  return {
    bindingSalt: req.bindingSalt,
    sandboxId: req.sandboxId,
    operationKind: RelayOperationKind.READ_FILE,
    operationId: req.readId,
    path: req.path,
  };
}

/** Binding of the first WriteFile call of a write_id. */
export function writeFileBinding(
  req: WriteFileRequest,
): RelayRequestBindingFields {
  return {
    bindingSalt: req.bindingSalt,
    sandboxId: req.sandboxId,
    operationKind: RelayOperationKind.WRITE_FILE,
    operationId: req.writeId,
    path: req.path,
  };
}

const textEncoder = new TextEncoder();

/**
 * Compares two strings by their UTF-8 bytes, the order that the contract
 * requires for `env` entries. JavaScript's default string order compares
 * UTF-16 code units, which differs for code points above U+FFFF.
 */
export function compareUtf8(a: string, b: string): number {
  return Buffer.compare(textEncoder.encode(a), textEncoder.encode(b));
}

/**
 * Returns the protobuf encoding of the binding with fields in field-number
 * order. Zero scalar fields are omitted, every element of `command` is kept
 * (also empty strings) and a present timeout is encoded even when it is zero.
 */
export function encodeRelayRequestBinding(
  b: RelayRequestBindingFields,
): Uint8Array {
  const w = new ProtoWriter();
  w.bytesField(1, b.bindingSalt);
  w.stringField(2, b.sandboxId);
  if (b.operationKind !== 0) {
    w.tag(3, WIRE_VARINT);
    w.varint(BigInt.asUintN(64, BigInt(b.operationKind)));
  }
  w.stringField(4, b.operationId);
  for (const arg of b.command ?? []) {
    w.tag(5, WIRE_BYTES);
    w.lengthDelimited(textEncoder.encode(arg));
  }
  const env = b.env ?? {};
  for (const key of Object.keys(env).sort(compareUtf8)) {
    const e = new ProtoWriter();
    e.stringField(1, key);
    e.stringField(2, env[key]!);
    w.tag(6, WIRE_BYTES);
    w.lengthDelimited(e.finish());
  }
  w.stringField(7, b.workingDirectory ?? "");
  if (b.timeout !== undefined) {
    const d = new ProtoWriter();
    const seconds = BigInt(b.timeout.seconds);
    if (seconds !== 0n) {
      d.tag(1, WIRE_VARINT);
      d.varint(BigInt.asUintN(64, seconds));
    }
    if (b.timeout.nanos !== 0) {
      d.tag(2, WIRE_VARINT);
      d.varint(BigInt.asUintN(64, BigInt(b.timeout.nanos)));
    }
    w.tag(8, WIRE_BYTES);
    w.lengthDelimited(d.finish());
  }
  w.stringField(9, b.path ?? "");
  return w.finish();
}

/** Returns the request digest: the SHA-256 of the binding encoding. */
export function requestDigest(b: RelayRequestBindingFields): Uint8Array {
  return new Uint8Array(
    createHash("sha256").update(encodeRelayRequestBinding(b)).digest(),
  );
}

const WIRE_VARINT = 0;
const WIRE_BYTES = 2;

/** A minimal protobuf writer for the explicit binding encoding. */
class ProtoWriter {
  readonly #parts: number[] = [];

  tag(field: number, wireType: number): void {
    this.varint(BigInt((field << 3) | wireType));
  }

  varint(value: bigint): void {
    let v = value;
    while (v >= 0x80n) {
      this.#parts.push(Number(v & 0x7fn) | 0x80);
      v >>= 7n;
    }
    this.#parts.push(Number(v));
  }

  lengthDelimited(bytes: Uint8Array): void {
    this.varint(BigInt(bytes.length));
    for (const byte of bytes) {
      this.#parts.push(byte);
    }
  }

  stringField(field: number, value: string): void {
    if (value === "") {
      return;
    }
    this.tag(field, WIRE_BYTES);
    this.lengthDelimited(textEncoder.encode(value));
  }

  bytesField(field: number, value: Uint8Array): void {
    if (value.length === 0) {
      return;
    }
    this.tag(field, WIRE_BYTES);
    this.lengthDelimited(value);
  }

  finish(): Uint8Array {
    return Uint8Array.from(this.#parts);
  }
}
