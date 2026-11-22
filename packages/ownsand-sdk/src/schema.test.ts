// SPDX-License-Identifier: Apache-2.0
import type { DescEnum, DescFile } from "@bufbuild/protobuf";
import { describe, expect, test } from "@rstest/core";
import { file_delino_ownsand_v1_agent } from "@delino/ownsand-protos/v1/agent_pb";
import { file_delino_ownsand_v1_api_key } from "@delino/ownsand-protos/v1/api_key_pb";
import { file_delino_ownsand_v1_audit } from "@delino/ownsand-protos/v1/audit_pb";
import {
  ErrorCode,
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
    expect(files.map((f) => f.name).sort()).toEqual(
      [
        "agent",
        "api_key",
        "audit",
        "common",
        "data_frame",
        "data_path",
        "device",
        "guest",
        "member",
        "organization",
        "relay",
        "sandbox",
        "snapshot",
      ].map((n) => `delino/ownsand/v1/${n}`),
    );
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
    expect(ErrorCode.CANCELED).toBe(1);
    expect(ErrorCode.UNAUTHENTICATED).toBe(16);
  });
});
