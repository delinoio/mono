// SPDX-License-Identifier: Apache-2.0
import { execFileSync } from "node:child_process";

delete process.env.NODE_OPTIONS;

execFileSync(
  process.execPath,
  [
    "--input-type=module",
    "--eval",
    `
      import assert from "node:assert/strict";
      import { EnvVar, ExecFailureReason, OwnSandClient, Role } from "ownsand-sdk";
      import { Role as ProtoRole } from "@delino/ownsand-protos/v1/common_pb";
      import { ExecFailureReason as ProtoExecFailureReason } from "@delino/ownsand-protos/v1/data_frame_pb";

      assert.equal(typeof OwnSandClient, "function");
      assert.equal(EnvVar.ApiKey, "OWNSAND_API_KEY");
      assert.equal(Role, ProtoRole);
      assert.equal(Role.OWNER, 1);
      assert.equal(ExecFailureReason, ProtoExecFailureReason);
    `,
  ],
  { cwd: new URL("../", import.meta.url), stdio: "inherit" },
);
