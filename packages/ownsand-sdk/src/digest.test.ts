// SPDX-License-Identifier: Apache-2.0
import { readFileSync } from "node:fs";
import { create, toBinary } from "@bufbuild/protobuf";
import { describe, expect, test } from "@rstest/core";
import {
  compareUtf8,
  encodeRelayRequestBinding,
  execBinding,
  newBindingSalt,
  type RelayRequestBindingFields,
  readFileBinding,
  requestDigest,
  writeFileBinding,
} from "./digest.ts";
import { RelayOperationKind } from "@delino/ownsand-protos/v1/common_pb";
import {
  ExecRequestSchema,
  ReadFileRequestSchema,
  RelayRequestBindingSchema,
  WriteFileRequestSchema,
} from "@delino/ownsand-protos/v1/data_frame_pb";

interface Vector {
  name: string;
  request: {
    kind: "exec" | "read_file" | "write_file";
    bindingSaltHex: string;
    sandboxId: string;
    operationId: string;
    command?: string[];
    env?: { key: string; value: string }[];
    workingDirectory?: string;
    timeout?: { seconds: string; nanos: number };
    path?: string;
  };
  sortedEnvKeys?: string[];
  encodedBindingHex: string;
  requestDigestHex: string;
}

// The Go relaybinding package owns these vectors; the TypeScript SDK must
// reproduce every one (docs/packages-ownsand-sdk-foundation.md).
const vectors: Vector[] = JSON.parse(
  readFileSync(
    new URL(
      "../../../go/ownsand/relaybinding/testdata/request-digest-vectors.json",
      import.meta.url,
    ),
    "utf8",
  ),
).vectors;

const KIND = {
  exec: RelayOperationKind.EXEC,
  read_file: RelayOperationKind.READ_FILE,
  write_file: RelayOperationKind.WRITE_FILE,
} as const;

function fieldsOf(v: Vector): RelayRequestBindingFields {
  const r = v.request;
  return {
    bindingSalt: Buffer.from(r.bindingSaltHex, "hex"),
    sandboxId: r.sandboxId,
    operationKind: KIND[r.kind],
    operationId: r.operationId,
    command: r.command,
    env:
      r.env === undefined
        ? undefined
        : Object.fromEntries(r.env.map((e) => [e.key, e.value])),
    workingDirectory: r.workingDirectory,
    timeout:
      r.timeout === undefined
        ? undefined
        : { seconds: BigInt(r.timeout.seconds), nanos: r.timeout.nanos },
    path: r.path,
  };
}

const hex = (b: Uint8Array) => Buffer.from(b).toString("hex");

describe("request digest golden vectors shared with Go", () => {
  test("the vector file has vectors of every operation kind", () => {
    const kinds = new Set(vectors.map((v) => v.request.kind));
    expect([...kinds].sort()).toEqual(["exec", "read_file", "write_file"]);
  });

  test.each(vectors.map((v) => [v.name, v] as const))(
    "%s: encoding and digest equal the Go output",
    (_name, vector) => {
      const fields = fieldsOf(vector);
      expect(hex(encodeRelayRequestBinding(fields))).toBe(
        vector.encodedBindingHex,
      );
      expect(hex(requestDigest(fields))).toBe(vector.requestDigestHex);
    },
  );

  test.each(
    vectors.filter((v) => v.sortedEnvKeys).map((v) => [v.name, v] as const),
  )("%s: env keys sort by UTF-8 bytes", (_name, vector) => {
    const keys = (vector.request.env ?? []).map((e) => e.key);
    expect([...keys].sort(compareUtf8)).toEqual(vector.sortedEnvKeys);
  });

  test.each(vectors.map((v) => [v.name, v] as const))(
    "%s: explicit encoding equals the protobuf-es encoding",
    (_name, vector) => {
      const f = fieldsOf(vector);
      const keys = Object.keys(f.env ?? {}).sort(compareUtf8);
      const message = create(RelayRequestBindingSchema, {
        bindingSalt: f.bindingSalt,
        sandboxId: f.sandboxId,
        operationKind: f.operationKind,
        operationId: f.operationId,
        command: f.command ? [...f.command] : [],
        env: keys.map((key) => ({ key, value: f.env![key]! })),
        workingDirectory: f.workingDirectory ?? "",
        timeout: f.timeout,
        path: f.path ?? "",
      });
      expect(hex(toBinary(RelayRequestBindingSchema, message))).toBe(
        vector.encodedBindingHex,
      );
    },
  );

  test("bindings built from requests equal the vectors", () => {
    for (const vector of vectors) {
      const f = fieldsOf(vector);
      let built: RelayRequestBindingFields;
      if (vector.request.kind === "exec") {
        built = execBinding(
          create(ExecRequestSchema, {
            sandboxId: f.sandboxId,
            execId: f.operationId,
            command: [...(f.command ?? [])],
            env: { ...f.env },
            workingDirectory: f.workingDirectory ?? "",
            timeout: f.timeout,
            bindingSalt: f.bindingSalt,
          }),
        );
      } else if (vector.request.kind === "read_file") {
        built = readFileBinding(
          create(ReadFileRequestSchema, {
            sandboxId: f.sandboxId,
            readId: f.operationId,
            path: f.path!,
            bindingSalt: f.bindingSalt,
            offset: 123n,
          }),
        );
      } else {
        built = writeFileBinding(
          create(WriteFileRequestSchema, {
            sandboxId: f.sandboxId,
            writeId: f.operationId,
            path: f.path!,
            bindingSalt: f.bindingSalt,
            data: new Uint8Array([1, 2, 3]),
          }),
        );
      }
      expect(hex(requestDigest(built))).toBe(vector.requestDigestHex);
    }
  });
});

describe("binding salt", () => {
  test("is 32 random bytes and new for each operation", () => {
    const a = newBindingSalt();
    const b = newBindingSalt();
    expect(a.length).toBe(32);
    expect(hex(a)).not.toBe(hex(b));
  });
});
