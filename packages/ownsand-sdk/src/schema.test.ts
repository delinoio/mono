// SPDX-License-Identifier: Apache-2.0
import { readdirSync } from "node:fs";
import { fileURLToPath } from "node:url";
import type { DescEnum, DescFile } from "@bufbuild/protobuf";
import { Code } from "@connectrpc/connect";
import { describe, expect, test } from "@rstest/core";
import { file_delino_ownsand_v1_agent } from "@delino/ownsand-protos/v1/agent_pb";
import { file_delino_ownsand_v1_api_key } from "@delino/ownsand-protos/v1/api_key_pb";
import { file_delino_ownsand_v1_audit } from "@delino/ownsand-protos/v1/audit_pb";
import { file_delino_ownsand_v1_billing } from "@delino/ownsand-protos/v1/billing_pb";
import {
  ErrorCodeSchema,
  file_delino_ownsand_v1_common,
} from "@delino/ownsand-protos/v1/common_pb";
import {
  DataFrameKind,
  DataFrameSchema,
  file_delino_ownsand_v1_data_frame,
} from "@delino/ownsand-protos/v1/data_frame_pb";
import { file_delino_ownsand_v1_data_path } from "@delino/ownsand-protos/v1/data_path_pb";
import { file_delino_ownsand_v1_device } from "@delino/ownsand-protos/v1/device_pb";
import { file_delino_ownsand_v1_guest } from "@delino/ownsand-protos/v1/guest_pb";
import { file_delino_ownsand_v1_member } from "@delino/ownsand-protos/v1/member_pb";
import { file_delino_ownsand_v1_organization } from "@delino/ownsand-protos/v1/organization_pb";
import { file_delino_ownsand_v1_relay } from "@delino/ownsand-protos/v1/relay_pb";
import { file_delino_ownsand_v1_sandbox } from "@delino/ownsand-protos/v1/sandbox_pb";
import { file_delino_ownsand_v1_snapshot } from "@delino/ownsand-protos/v1/snapshot_pb";

const files: DescFile[] = [
  file_delino_ownsand_v1_agent,
  file_delino_ownsand_v1_api_key,
  file_delino_ownsand_v1_audit,
  file_delino_ownsand_v1_billing,
  file_delino_ownsand_v1_common,
  file_delino_ownsand_v1_data_frame,
  file_delino_ownsand_v1_data_path,
  file_delino_ownsand_v1_device,
  file_delino_ownsand_v1_guest,
  file_delino_ownsand_v1_member,
  file_delino_ownsand_v1_organization,
  file_delino_ownsand_v1_relay,
  file_delino_ownsand_v1_sandbox,
  file_delino_ownsand_v1_snapshot,
];

const protoDir = fileURLToPath(
  new URL("../../../protos/delino/ownsand/v1/", import.meta.url),
);

function enumsOf(file: DescFile): DescEnum[] {
  const out: DescEnum[] = [...file.enums];
  const visit = (messages: DescFile["messages"]) => {
    for (const m of messages) {
      out.push(...m.nestedEnums);
      visit(m.nestedMessages);
    }
  };
  visit(file.messages);
  return out;
}

describe("generated TypeScript bindings", () => {
  test("cover every schema file of delino.ownsand.v1", () => {
    const fileNames = readdirSync(protoDir, { withFileTypes: true })
      .filter((entry) => entry.isFile())
      .map((entry) => entry.name)
      .sort();
    const prefix = "delino/ownsand/v1/";
    const expected = fileNames
      .filter((name) => name.endsWith(".proto"))
      .map((name) => prefix + name.slice(0, -".proto".length));
    const bindings = fileNames
      .filter((name) => name.endsWith("_pb.ts"))
      .map((name) => prefix + name.slice(0, -"_pb.ts".length));
    const registered = files.map((file) => file.name).sort();
    const duplicateDescriptors = [
      ...new Set(
        registered.filter((name, index) => registered.indexOf(name) !== index),
      ),
    ];

    expect({
      missingBindings: expected.filter((name) => !bindings.includes(name)),
      unexpectedBindings: bindings.filter((name) => !expected.includes(name)),
      missingDescriptors: expected.filter((name) => !registered.includes(name)),
      extraDescriptors: registered.filter((name) => !expected.includes(name)),
      duplicateDescriptors,
    }).toEqual({
      missingBindings: [],
      unexpectedBindings: [],
      missingDescriptors: [],
      extraDescriptors: [],
      duplicateDescriptors: [],
    });
  });

  test("every enum has the zero value <ENUM>_UNSPECIFIED", () => {
    for (const file of files) {
      for (const e of enumsOf(file)) {
        const zero = e.values.find((v) => v.number === 0);
        expect(zero?.name).toBe(
          `${e.sharedPrefix?.toUpperCase() ?? ""}UNSPECIFIED`,
        );
      }
    }
  });

  test("each DataFrameKind names exactly one payload field", () => {
    const fields = DataFrameSchema.oneofs.find(
      (o) => o.name === "payload",
    )!.fields;
    const kinds = Object.values(DataFrameKind).filter(
      (v): v is number => typeof v === "number" && v !== 0,
    );
    expect(fields.map((f) => f.name.toUpperCase()).sort()).toEqual(
      kinds.map((k) => DataFrameKind[k]).sort(),
    );
  });

  test("ErrorCode values equal the Connect codes", () => {
    const actual = Object.fromEntries(
      ErrorCodeSchema.values
        .filter((value) => value.number !== 0)
        .map((value) => [value.name, value.number]),
    );
    const expected = Object.fromEntries(
      Object.entries<string | number>(Code)
        .filter(([, number]) => typeof number === "number" && number !== 0)
        .map(([name, number]) => [
          `ERROR_CODE_${name.replace(/([a-z])([A-Z])/g, "$1_$2").toUpperCase()}`,
          number,
        ]),
    );
    expect(actual).toEqual(expected);
  });
});
