// SPDX-License-Identifier: Apache-2.0
import { createHash } from "node:crypto";

import { clone, create, toBinary } from "@bufbuild/protobuf";

import { RelayOperationKind } from "./gen/delino/ownsand/v1/common_pb.ts";
import {
  type ExecRequest,
  type RelayRequestBinding,
  RelayRequestBinding_EnvEntrySchema,
  RelayRequestBindingSchema,
} from "./gen/delino/ownsand/v1/datapath_pb.ts";

const utf8 = new TextEncoder();

/** Compares two strings by their UTF-8 bytes, like Go string comparison. */
export function compareUtf8(a: string, b: string): number {
  const x = utf8.encode(a);
  const y = utf8.encode(b);
  const n = Math.min(x.length, y.length);
  for (let i = 0; i < n; i++) {
    if (x[i] !== y[i]) {
      return x[i]! - y[i]!;
    }
  }
  return x.length - y.length;
}

/** Builds the `RelayRequestBinding` of an exec request. Env entries are sorted by key bytes. */
export function execBinding(req: ExecRequest): RelayRequestBinding {
  const keys = Object.keys(req.env).sort(compareUtf8);
  return create(RelayRequestBindingSchema, {
    bindingSalt: req.bindingSalt.slice(),
    sandboxId: req.sandboxId,
    operationKind: RelayOperationKind.EXEC,
    operationId: req.execId,
    command: [...req.command],
    env: keys.map((key) =>
      create(RelayRequestBinding_EnvEntrySchema, { key, value: req.env[key]! }),
    ),
    workingDirectory: req.workingDirectory,
    timeout: req.timeout
      ? { seconds: req.timeout.seconds, nanos: req.timeout.nanos }
      : undefined,
  });
}

/** Builds the `RelayRequestBinding` of a file read. */
export function readBinding(
  salt: Uint8Array,
  sandboxId: string,
  readId: string,
  path: string,
): RelayRequestBinding {
  return create(RelayRequestBindingSchema, {
    bindingSalt: salt.slice(),
    sandboxId,
    operationKind: RelayOperationKind.READ_FILE,
    operationId: readId,
    path,
  });
}

/** Builds the `RelayRequestBinding` of a file write. */
export function writeBinding(
  salt: Uint8Array,
  sandboxId: string,
  writeId: string,
  path: string,
): RelayRequestBinding {
  return create(RelayRequestBindingSchema, {
    bindingSalt: salt.slice(),
    sandboxId,
    operationKind: RelayOperationKind.WRITE_FILE,
    operationId: writeId,
    path,
  });
}

/**
 * Returns the protobuf binary encoding of a binding that the request digest
 * hashes: known fields in field-number order and no unknown fields.
 */
export function encodeBinding(binding: RelayRequestBinding): Uint8Array {
  // protobuf-es writes fields in descriptor order, which for this message is
  // field-number order. Unknown fields are dropped through a clone without
  // `$unknown` and the writer option.
  const clean = clone(RelayRequestBindingSchema, binding);
  delete clean.$unknown;
  for (const e of clean.env) {
    delete e.$unknown;
  }
  if (clean.timeout) {
    delete clean.timeout.$unknown;
  }
  return toBinary(RelayRequestBindingSchema, clean, {
    writeUnknownFields: false,
  });
}

/** Returns the SHA-256 request digest of a binding. */
export function bindingDigest(binding: RelayRequestBinding): Uint8Array {
  return new Uint8Array(
    createHash("sha256").update(encodeBinding(binding)).digest(),
  );
}
