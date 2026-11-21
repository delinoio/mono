// SPDX-License-Identifier: Apache-2.0
import type { DescEnum, DescFile } from "@bufbuild/protobuf";
import { describe, expect, test } from "@rstest/core";
import { file_delino_ownsand_v1_agent } from "./gen/delino/ownsand/v1/agent_pb.ts";
import { file_delino_ownsand_v1_api_key } from "./gen/delino/ownsand/v1/api_key_pb.ts";
import { file_delino_ownsand_v1_audit } from "./gen/delino/ownsand/v1/audit_pb.ts";
import {
  ErrorCode,
  file_delino_ownsand_v1_common,
} from "./gen/delino/ownsand/v1/common_pb.ts";
import {
  DataFrameKind,
  DataFrameSchema,
  file_delino_ownsand_v1_data_frame,
} from "./gen/delino/ownsand/v1/data_frame_pb.ts";
import { file_delino_ownsand_v1_data_path } from "./gen/delino/ownsand/v1/data_path_pb.ts";
import { file_delino_ownsand_v1_device } from "./gen/delino/ownsand/v1/device_pb.ts";
import { file_delino_ownsand_v1_guest } from "./gen/delino/ownsand/v1/guest_pb.ts";
import { file_delino_ownsand_v1_member } from "./gen/delino/ownsand/v1/member_pb.ts";
import { file_delino_ownsand_v1_organization } from "./gen/delino/ownsand/v1/organization_pb.ts";
import { file_delino_ownsand_v1_relay } from "./gen/delino/ownsand/v1/relay_pb.ts";
import { file_delino_ownsand_v1_sandbox } from "./gen/delino/ownsand/v1/sandbox_pb.ts";
import { file_delino_ownsand_v1_snapshot } from "./gen/delino/ownsand/v1/snapshot_pb.ts";

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
