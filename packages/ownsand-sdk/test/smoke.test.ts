// SPDX-License-Identifier: Apache-2.0
import { describe, expect, test } from "@rstest/core";

import { ExecResultKind } from "../src/relay/exec.ts";
import { harness, text } from "./support/harness.ts";

describe("smoke", () => {
  test("exec, read and write over the relay", async () => {
    const h = harness();
    h.fake.programs.set("echo hi", [
      { delayMs: 10, stdout: "hi\n" },
      { delayMs: 10, exit: 3 },
    ]);
    const { result, stdout } = await h.run(
      h.sandbox.exec(["echo", "hi"]).collect(),
    );
    expect(result.kind).toBe(ExecResultKind.EXITED);
    expect(result.kind === ExecResultKind.EXITED && result.exitCode).toBe(3n);
    expect(text(stdout)).toBe("hi\n");

    await h.run(h.sandbox.writeFile("/tmp/a.txt", "hello world"));
    expect(text(h.fake.files.get("/tmp/a.txt")!)).toBe("hello world");
    const back = await h.run(h.sandbox.readFile("/tmp/a.txt").bytes());
    expect(text(back)).toBe("hello world");
    expect(h.fake.violations).toEqual([]);
  });
});
