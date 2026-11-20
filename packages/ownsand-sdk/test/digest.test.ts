// SPDX-License-Identifier: Apache-2.0
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { create, fromBinary } from "@bufbuild/protobuf";
import { durationFromMs } from "@bufbuild/protobuf/wkt";
import { describe, expect, test } from "@rstest/core";

import { RelayOperationKind } from "../src/gen/delino/ownsand/v1/common_pb.ts";
import {
  ExecRequestSchema,
  RelayRequestBindingSchema,
} from "../src/gen/delino/ownsand/v1/datapath_pb.ts";
import {
  bindingDigest,
  compareUtf8,
  encodeBinding,
  execBinding,
  readBinding,
  writeBinding,
} from "../src/digest.ts";

interface DigestVector {
  name: string;
  binding_hex: string;
  digest_hex: string;
}

// The Go datapath package pins these vectors; this is the required test that
// the Go and TypeScript encodings of the same binding give the same digest.
const vectorsPath = fileURLToPath(
  new URL(
    "../../ownsand-sdk-go/datapath/testdata/digest_vectors.json",
    import.meta.url,
  ),
);
const vectors = JSON.parse(readFileSync(vectorsPath, "utf8")) as DigestVector[];

const hex = (b: Uint8Array) => Buffer.from(b).toString("hex");
const salt = new Uint8Array(32).fill(0xab);
const sandbox = "0190f5d6-1c2b-7a3e-8f00-000000000001";

// The same inputs as vectorBindings() in packages/ownsand-sdk-go/datapath/datapath_test.go.
const bindings = {
  exec: execBinding(
    create(ExecRequestSchema, {
      sandboxId: sandbox,
      execId: "0190f5d6-1c2b-7a3e-8f00-000000000002",
      command: ["/bin/sh", "-c", "echo héllo"],
      // Insertion order differs from the sorted order on purpose.
      env: { É: "utf8", a: "lower", Z: "last", A: "first" },
      workingDirectory: "/work",
      timeout: durationFromMs(90_005),
      bindingSalt: salt,
    }),
  ),
  exec_minimal: execBinding(
    create(ExecRequestSchema, {
      sandboxId: sandbox,
      execId: "0190f5d6-1c2b-7a3e-8f00-000000000003",
      command: ["true"],
      bindingSalt: salt,
    }),
  ),
  read: readBinding(
    salt,
    sandbox,
    "0190f5d6-1c2b-7a3e-8f00-000000000004",
    "/etc/hostname",
  ),
  write: writeBinding(
    salt,
    sandbox,
    "0190f5d6-1c2b-7a3e-8f00-000000000005",
    "C:\\Users\\a.txt",
  ),
};

describe("request digest", () => {
  test("covers every Go vector", () => {
    expect(vectors.map((v) => v.name).sort()).toEqual(
      Object.keys(bindings).sort(),
    );
  });

  for (const vector of vectors) {
    test(`matches the Go vector ${vector.name}`, () => {
      const binding = bindings[vector.name as keyof typeof bindings];
      expect(hex(encodeBinding(binding))).toBe(vector.binding_hex);
      expect(hex(bindingDigest(binding))).toBe(vector.digest_hex);
    });

    test(`re-encodes the decoded Go binding ${vector.name} identically`, () => {
      const decoded = fromBinary(
        RelayRequestBindingSchema,
        Buffer.from(vector.binding_hex, "hex"),
      );
      expect(hex(bindingDigest(decoded))).toBe(vector.digest_hex);
    });
  }

  test("ignores unknown fields", () => {
    const withUnknown = fromBinary(
      RelayRequestBindingSchema,
      Buffer.concat([
        Buffer.from(vectors[0]!.binding_hex, "hex"),
        Buffer.from([0xf8, 0x06, 0x01]),
      ]),
    );
    expect(withUnknown.$unknown?.length).toBe(1);
    expect(hex(bindingDigest(withUnknown))).toBe(vectors[0]!.digest_hex);
  });

  test("sorts env keys by UTF-8 bytes", () => {
    expect(["É", "a", "Z", "A"].sort(compareUtf8)).toEqual([
      "A",
      "Z",
      "a",
      "É",
    ]);
    expect(bindings.exec.env.map((e) => e.key)).toEqual(["A", "Z", "a", "É"]);
    expect(bindings.exec.operationKind).toBe(RelayOperationKind.EXEC);
  });
});
